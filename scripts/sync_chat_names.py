#!/usr/bin/env python3
"""Pone nombre a los chats de WhatsApp que solo tienen el numero como nombre.

El puente guarda `chats.name` = numero cuando WhatsApp no entrega nombre (LIDs, contactos
sin push name). Como el MCP busca por ese campo, `list_chats(query="aldin")` no los encuentra.
Este script cruza, por prioridad:
  1. Contactos de macOS (nombre que el usuario eligio), por telefono normalizado.
  2. whatsmeow_contacts: full_name > first_name > push_name, via whatsmeow_lid_map (lid -> telefono).
y escribe el resultado en chats.name. El puente reutiliza el nombre existente, asi que persiste.

Uso:  sync_chat_names.py           prueba: lista lo que cambiaria, no escribe
      sync_chat_names.py --apply   escribe
Solo toca chats cuyo nombre actual es el propio numero/JID; nunca pisa un nombre real.
"""
from __future__ import annotations

import argparse
import re
import sqlite3
import subprocess
import sys
from pathlib import Path

STORE = Path(__file__).resolve().parent.parent / "whatsapp-bridge" / "store"
OSA = '''tell application "Contacts"
set ns to name of every person
set ps to value of phones of every person
end tell
set out to ""
repeat with i from 1 to count of ns
  repeat with ph in item i of ps
    set out to out & item i of ns & tab & ph & linefeed
  end repeat
end repeat
return out'''


def key(phone: str) -> str | None:
    """Clave de cruce: ultimos 10 digitos (absorbe +58 / 0414 / 58414 sin una tabla por pais)."""
    d = re.sub(r"\D", "", phone)
    return d[-10:] if len(d) >= 10 else None


def mac_contacts() -> dict[str, str]:
    if sys.platform != "darwin":
        return {}
    try:  # sin permiso de Contactos (TCC) osascript espera un dialogo: no colgar la tarea
        r = subprocess.run(["osascript", "-e", OSA], capture_output=True, text=True, timeout=120)
    except subprocess.TimeoutExpired:
        print("aviso: Contactos de macOS sin respuesta en 120s (permiso de privacidad pendiente); se usan solo nombres de WhatsApp", file=sys.stderr)
        return {}
    if r.returncode != 0:
        print(f"aviso: Contactos de macOS no disponible ({r.stderr.strip()[:80]})", file=sys.stderr)
        return {}
    out: dict[str, str] = {}
    for line in r.stdout.splitlines():
        name, _, phone = line.partition("\t")
        k = key(phone)
        if k and name.strip():
            out.setdefault(k, name.strip())
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--apply", action="store_true")
    args = ap.parse_args()

    wa = sqlite3.connect(f"file:{STORE / 'whatsapp.db'}?mode=ro", uri=True)
    lid2pn = dict(wa.execute("select lid, pn from whatsmeow_lid_map"))
    wa_names: dict[str, str] = {}
    for jid, full, first, push in wa.execute(
        "select their_jid, full_name, first_name, push_name from whatsmeow_contacts"
    ):
        n = (full or first or push or "").strip()
        if n:
            wa_names[jid.split("@")[0]] = n
    mac = mac_contacts()

    db = sqlite3.connect(STORE / "messages.db")
    todo = []
    for jid, name in db.execute("select jid, name from chats where jid not like '%@g.us'"):
        ident = jid.split("@")[0]
        if name != ident:  # ya tiene nombre real: no se pisa
            continue
        phone = lid2pn.get(ident, ident if jid.endswith("@s.whatsapp.net") else None)
        k = key(phone) if phone else None
        new, src = (mac.get(k), "macos") if k and mac.get(k) else (None, None)
        if not new and phone and wa_names.get(phone):
            new, src = wa_names[phone], "whatsapp"
        if not new and wa_names.get(ident):
            new, src = wa_names[ident], "whatsapp"
        if new and len(re.findall(r"[^\W\d_]", new)) >= 2:  # sin letras (emoji, simbolos) no es un nombre
            todo.append((jid, name, new, src))

    unresolved = db.execute(
        "select count(*) from chats where jid not like '%@g.us' and name = substr(jid,1,instr(jid,'@')-1)"
    ).fetchone()[0]
    for jid, old, new, src in todo:
        print(f"{src:8} {old} -> {new}")
    print(f"\nresolubles: {len(todo)} de {unresolved} chats sin nombre (sin grupos)")
    if args.apply and todo:
        db.executemany("update chats set name=? where jid=?", [(n, j) for j, _, n, _ in todo])
        db.commit()
        print("aplicado")
    return 0


if __name__ == "__main__":
    sys.exit(main())
