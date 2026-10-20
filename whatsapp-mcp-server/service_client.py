"""Client for the remote WhatsApp bridge service (gateway + Cloudflare Access).

Service mode is enabled when WA_SERVICE_URL is set. In that mode the MCP server never reads
the local messages.db nor local files: every operation goes through the service REST API.

Security notes:
- Credentials come only from the environment and are sent only as request headers.
- Error messages returned to the model are generic (HTTP status + short text). They never
  include headers, tokens, the service URL, or the response body.
- POST /api/send is never retried.
"""

import os
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import urlsplit

import requests

# (connect, read) timeouts in seconds
TIMEOUT = (5, 20)

NOT_AVAILABLE = "not available in service mode"
NOT_AVAILABLE_FILES = (
    "not available in service mode: local files are not read and client file paths "
    "are not sent to the service"
)
DENIED = "service denied this operation (check profile credentials)"

_REQUIRED_ENV = ("WA_SERVICE_TOKEN", "CF_ACCESS_CLIENT_ID", "CF_ACCESS_CLIENT_SECRET")
_LOCAL_HOSTS = {"localhost", "127.0.0.1"}


class ServiceError(Exception):
    """Error safe to show to the model (contains no secrets)."""


def validate_base_url(url: str) -> str:
    """Return the normalised base URL or raise ServiceError."""
    url = url.rstrip("/")
    parts = urlsplit(url)
    if "?" in url or "#" in url or "@" in parts.netloc or parts.path not in ("", "/"):
        raise ServiceError("WA_SERVICE_URL must be a plain base URL")
    if parts.scheme == "https" and parts.hostname:
        return url
    if parts.scheme == "http" and parts.hostname in _LOCAL_HOSTS:
        return url
    raise ServiceError("WA_SERVICE_URL must use https:// (http:// only for localhost)")


class ServiceClient:
    def __init__(self, base_url: str):
        self.base_url = validate_base_url(base_url)

    def _headers(self) -> Dict[str, str]:
        missing = [name for name in _REQUIRED_ENV if not os.environ.get(name)]
        if missing:
            raise ServiceError("service mode requires these environment variables: " + ", ".join(missing))
        return {
            "Authorization": f"Bearer {os.environ['WA_SERVICE_TOKEN']}",
            "CF-Access-Client-Id": os.environ["CF_ACCESS_CLIENT_ID"],
            "CF-Access-Client-Secret": os.environ["CF_ACCESS_CLIENT_SECRET"],
        }

    def _request(self, method: str, path: str, params: Optional[Dict[str, Any]] = None,
                 payload: Optional[Dict[str, Any]] = None) -> Any:
        headers = self._headers()
        try:
            # Single attempt: no retries for any method (and never for /api/send)
            resp = requests.request(method, self.base_url + path, params=params, json=payload,
                                    headers=headers, timeout=TIMEOUT, allow_redirects=False)
        except requests.Timeout:
            raise ServiceError("service request timed out") from None
        except requests.RequestException:
            raise ServiceError("service unreachable") from None
        if resp.status_code in (401, 403):
            raise ServiceError(DENIED)
        if resp.status_code != 200:
            raise ServiceError(f"service error: HTTP {resp.status_code}")
        try:
            return resp.json()
        except ValueError:
            raise ServiceError("service returned an invalid response") from None

    # Read operations

    def list_chats(self, query: Optional[str] = None, limit: int = 20) -> List[Dict[str, Any]]:
        params: Dict[str, Any] = {"limit": limit}
        if query:
            params["query"] = query
        return self._request("GET", "/api/chats", params=params)

    def list_messages(self, chat_jid: Optional[str] = None, before: Optional[str] = None,
                      query: Optional[str] = None, limit: int = 20) -> List[Dict[str, Any]]:
        if not chat_jid:
            if query:
                return self.search_messages(query, limit)
            raise ServiceError("in service mode list_messages requires chat_jid (or query to search all chats)")
        params: Dict[str, Any] = {"chat_jid": chat_jid, "limit": limit}
        if before:
            params["before"] = before
        if query:
            params["q"] = query
        return self._request("GET", "/api/messages", params=params)

    def search_messages(self, query: str, limit: int = 20) -> List[Dict[str, Any]]:
        return self._request("GET", "/api/search", params={"q": query, "limit": limit})

    def search_contacts(self, query: str) -> List[Dict[str, Any]]:
        chats = self._request("GET", "/api/chats", params={"query": query, "limit": 50})
        return [
            {"phone_number": c["jid"].split("@")[0], "name": c.get("name"), "jid": c["jid"]}
            for c in chats
            if isinstance(c.get("jid"), str) and c["jid"] and not c["jid"].endswith("@g.us")
        ]

    # Write operations (results keep the legacy tool shapes)

    def send_message(self, recipient: str, message: str) -> Tuple[bool, str]:
        try:
            result = self._request("POST", "/api/send", payload={"recipient": recipient, "message": message})
        except ServiceError as e:
            return False, str(e)
        return bool(result.get("success", False)), str(result.get("message", "Unknown response"))

    def download_media(self, message_id: str, chat_jid: str) -> Dict[str, Any]:
        try:
            result = self._request("POST", "/api/download",
                                   payload={"message_id": message_id, "chat_jid": chat_jid})
        except ServiceError as e:
            return {"success": False, "message": str(e)}
        if result.get("success"):
            return {"success": True, "message": "Media downloaded successfully (path is on the service host)",
                    "file_path": result.get("path")}
        return {"success": False, "message": "Failed to download media"}


def get_service_client() -> Optional[ServiceClient]:
    """Single decision point: a ServiceClient in service mode, None in legacy mode."""
    url = os.environ.get("WA_SERVICE_URL")
    if not url:
        return None
    return ServiceClient(url)
