import json
import logging
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import main  # noqa: E402
import service_client  # noqa: E402
import whatsapp  # noqa: E402

TOKEN = "SENTINEL-TOKEN-7f3a9"
CF_ID = "SENTINEL-CFID-2b81"
CF_SECRET = "SENTINEL-CFSECRET-c4d0"

# Shapes from whatsapp-bridge/readapi.go
CHATS = [
    {"jid": "34600111222@s.whatsapp.net", "name": "Alice", "last_message_time": "2026-10-19T10:00:00Z"},
    {"jid": "120363000000@g.us", "name": "Family"},
]
MESSAGES = [
    {"id": "M1", "chat_jid": "34600111222@s.whatsapp.net", "sender": "34600111222",
     "sender_name": "Alice", "content": "hola", "timestamp": "2026-10-19T10:00:00Z",
     "is_from_me": False, "media_type": "image", "filename": "a.jpg"},
    {"id": "M2", "chat_jid": "34600111222@s.whatsapp.net", "sender": "me",
     "content": "hey", "is_from_me": True},
]


class FakeService:
    def __init__(self):
        self.requests = []
        self.status = 200
        self.body = None
        self.raw_body = None
        self.location = None
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def _handle(self):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length) if length else b""
                parts = urlsplit(self.path)
                outer.requests.append({
                    "method": self.command, "path": parts.path,
                    "query": parse_qs(parts.query), "headers": dict(self.headers),
                    "json": json.loads(raw) if raw else None,
                })
                if outer.raw_body is not None:
                    body = outer.raw_body
                elif outer.status != 200:
                    body = f"upstream failure {TOKEN} {CF_SECRET}".encode()
                else:
                    body = json.dumps(outer.body if outer.body is not None else self._default(parts.path)).encode()
                self.send_response(outer.status)
                if outer.location:
                    self.send_header("Location", outer.location)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def _default(self, path):
                return {"/api/chats": CHATS, "/api/messages": MESSAGES, "/api/search": MESSAGES[:1],
                        "/api/send": {"success": True, "message": "Message sent"},
                        "/api/download": {"success": True, "path": "/data/media/a.jpg"}}.get(path, {})

            do_GET = _handle
            do_POST = _handle

            def log_message(self, *args):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()


@pytest.fixture
def svc(monkeypatch):
    fake = FakeService()
    monkeypatch.setenv("WA_SERVICE_URL", fake.url + "/")
    monkeypatch.setenv("WA_SERVICE_TOKEN", TOKEN)
    monkeypatch.setenv("CF_ACCESS_CLIENT_ID", CF_ID)
    monkeypatch.setenv("CF_ACCESS_CLIENT_SECRET", CF_SECRET)
    yield fake
    fake.close()


def assert_no_secrets(*outputs):
    text = " ".join(str(o) for o in outputs)
    for secret in (TOKEN, CF_ID, CF_SECRET):
        assert secret not in text


def test_legacy_mode_without_service_url(monkeypatch):
    monkeypatch.delenv("WA_SERVICE_URL", raising=False)
    assert service_client.get_service_client() is None
    calls = []
    monkeypatch.setattr(main, "whatsapp_list_chats", lambda **kw: calls.append(kw) or ["legacy"])
    monkeypatch.setattr(main, "whatsapp_send_message", lambda r, m: (True, "legacy sent"))
    assert main.list_chats(query="x") == ["legacy"]
    assert main.send_message("123", "hi") == {"success": True, "message": "legacy sent"}
    assert whatsapp.WHATSAPP_API_BASE_URL == "http://localhost:8080/api"


def test_sends_three_auth_headers(svc):
    main.list_chats()
    h = svc.requests[0]["headers"]
    assert h["Authorization"] == f"Bearer {TOKEN}"
    assert h["CF-Access-Client-Id"] == CF_ID
    assert h["CF-Access-Client-Secret"] == CF_SECRET


def test_list_chats(svc):
    assert main.list_chats(query="Ali", limit=5) == CHATS
    r = svc.requests[0]
    assert (r["method"], r["path"], r["query"]) == ("GET", "/api/chats", {"query": ["Ali"], "limit": ["5"]})


def test_list_messages(svc):
    out = main.list_messages(chat_jid="34600111222@s.whatsapp.net", before="2026-10-19T00:00:00Z", query="ho")
    assert out == MESSAGES
    r = svc.requests[0]
    assert r["path"] == "/api/messages"
    assert r["query"] == {"chat_jid": ["34600111222@s.whatsapp.net"], "before": ["2026-10-19T00:00:00Z"],
                          "q": ["ho"], "limit": ["20"]}


def test_search_messages(svc):
    out = main.search_messages("hola", limit=3)
    assert out[0]["sender_name"] == "Alice" and out[0]["media_type"] == "image"
    r = svc.requests[0]
    assert (r["path"], r["query"]) == ("/api/search", {"q": ["hola"], "limit": ["3"]})


def test_search_contacts_excludes_groups(svc):
    out = main.search_contacts("a")
    assert out == [{"phone_number": "34600111222", "name": "Alice", "jid": "34600111222@s.whatsapp.net"}]
    assert svc.requests[0]["query"]["query"] == ["a"]


def test_send_message_payload(svc):
    assert main.send_message("34600111222", "hola") == {"success": True, "message": "Message sent"}
    r = svc.requests[0]
    assert (r["method"], r["path"], r["json"]) == ("POST", "/api/send", {"recipient": "34600111222", "message": "hola"})


def test_send_message_not_retried_on_500(svc):
    svc.status = 500
    out = main.send_message("34600111222", "hola")
    assert out == {"success": False, "message": "service error: HTTP 500"}
    assert len(svc.requests) == 1
    assert_no_secrets(out)


def test_403_service_denied(svc):
    svc.status = 403
    out = main.send_message("34600111222", "hola")
    assert out["message"] == "service denied this operation (check profile credentials)"
    with pytest.raises(service_client.ServiceError, match="service denied"):
        main.list_chats()


def test_download_media(svc):
    out = main.download_media("M1", "34600111222@s.whatsapp.net")
    assert out["success"] and out["file_path"] == "/data/media/a.jpg"
    assert svc.requests[0]["json"] == {"message_id": "M1", "chat_jid": "34600111222@s.whatsapp.net"}


@pytest.mark.parametrize("call", [
    lambda: main.get_chat("x@s.whatsapp.net"),
    lambda: main.get_direct_chat_by_contact("123"),
    lambda: main.get_contact_chats("x@s.whatsapp.net"),
    lambda: main.get_last_interaction("x@s.whatsapp.net"),
    lambda: main.get_message_context("M1"),
])
def test_local_only_tools_unavailable(svc, call):
    with pytest.raises(service_client.ServiceError) as e:
        call()
    assert str(e.value) == "not available in service mode"
    assert svc.requests == []


def test_file_tools_unavailable(svc, tmp_path):
    f = tmp_path / "a.ogg"
    f.write_bytes(b"x")
    for tool in (main.send_file, main.send_audio_message):
        out = tool("123", str(f))
        assert out["success"] is False
        assert out["message"].startswith("not available in service mode")
        assert "local files are not read" in out["message"]
    assert svc.requests == []


def test_errors_never_leak_secrets(svc, caplog, capsys):
    caplog.set_level(logging.DEBUG)
    outputs = []
    for status in (400, 401, 403, 404, 500, 502):
        svc.status = status
        outputs.append(main.send_message("1", "x"))
        outputs.append(main.download_media("M1", "c"))
        with pytest.raises(service_client.ServiceError) as e:
            main.list_chats()
        outputs.append(str(e.value))
    captured = capsys.readouterr()
    assert_no_secrets(*outputs, caplog.text, captured.out, captured.err)
    assert all(svc.url not in str(o) for o in outputs)


def test_unreachable_service_is_generic(monkeypatch):
    monkeypatch.setenv("WA_SERVICE_URL", "http://127.0.0.1:9")
    monkeypatch.setenv("WA_SERVICE_TOKEN", TOKEN)
    monkeypatch.setenv("CF_ACCESS_CLIENT_ID", CF_ID)
    monkeypatch.setenv("CF_ACCESS_CLIENT_SECRET", CF_SECRET)
    out = main.send_message("1", "x")
    assert out == {"success": False, "message": "service unreachable"}


def test_missing_credentials_fail_clearly(monkeypatch):
    monkeypatch.setenv("WA_SERVICE_URL", "https://example.invalid")
    monkeypatch.setenv("WA_SERVICE_TOKEN", TOKEN)
    monkeypatch.delenv("CF_ACCESS_CLIENT_ID", raising=False)
    monkeypatch.delenv("CF_ACCESS_CLIENT_SECRET", raising=False)
    with pytest.raises(service_client.ServiceError) as e:
        main.list_chats()
    assert "CF_ACCESS_CLIENT_ID" in str(e.value) and "CF_ACCESS_CLIENT_SECRET" in str(e.value)
    assert_no_secrets(str(e.value))


@pytest.mark.parametrize("url", ["http://wa.example.com", "ftp://x", "https://user:pw@wa.example.com", "wa.example.com",
    "https://wa.example.com/api/send#", "https://wa.example.com?", "https://wa.example.com/?x=1",
    "https://:@wa.example.com", "https://@wa.example.com", "https://wa.example.com/api",
    "https://wa.example.com/../x"])
def test_rejects_insecure_urls(monkeypatch, url):
    monkeypatch.setenv("WA_SERVICE_URL", url)
    with pytest.raises(service_client.ServiceError):
        main.list_chats()


def test_timeouts_are_explicit():
    assert service_client.TIMEOUT == (5, 20)


def test_accepts_plain_base_urls():
    assert service_client.validate_base_url("https://wa.example.com/") == "https://wa.example.com"
    assert service_client.validate_base_url("http://localhost:8080") == "http://localhost:8080"


def test_redirect_not_followed(svc):
    other = FakeService()
    try:
        svc.status = 302
        svc.location = other.url + "/api/send"
        out = main.send_message("1", "x")
        with pytest.raises(service_client.ServiceError) as e:
            main.list_chats()
        assert other.requests == []
        assert out == {"success": False, "message": "service error: HTTP 302"}
        assert_no_secrets(out, str(e.value))
    finally:
        other.close()


def test_non_json_200_is_generic(svc):
    svc.raw_body = f"<html>oops {TOKEN}</html>".encode()
    with pytest.raises(service_client.ServiceError) as e:
        main.list_chats()
    assert str(e.value) == "service returned an invalid response"
    out = main.send_message("1", "x")
    assert out == {"success": False, "message": "service returned an invalid response"}
    assert "oops" not in str(out)


def test_search_contacts_skips_chats_without_jid(svc):
    svc.body = [{"name": "NoJid"}, {"jid": None, "name": "Null"}, CHATS[0]]
    assert main.search_contacts("a") == [
        {"phone_number": "34600111222", "name": "Alice", "jid": "34600111222@s.whatsapp.net"}]
