#!/usr/bin/env python3
"""Local one-page signer for the network wallet consent.

Serves ONE page on 127.0.0.1 that asks the Talisman browser extension to sign
the consent message, then saves the signature (a public value) to a file.

It never sees a seed phrase or a private key: the extension signs, this page only
passes the message in and the signature out.

usage: sign_server.py <address> <message_file> <signature_out_file> [port]
"""
import http.server
import json
import os
import sys
import threading
import time

address, message_file, out_file = sys.argv[1], sys.argv[2], sys.argv[3]
port = int(sys.argv[4]) if len(sys.argv) > 4 else 8765
with open(message_file, "r", encoding="utf-8") as f:
    message = f.read()
message_hex = "0x" + message.encode("utf-8").hex()
deadline = time.time() + 6 * 60

PAGE = """<!doctype html>
<html><head><meta charset="utf-8"><title>Sign the URnetwork wallet consent</title>
<style>
body{font-family:system-ui,sans-serif;max-width:46rem;margin:2rem auto;padding:0 1rem;line-height:1.5}
pre{background:#f4f4f4;padding:.75rem;overflow:auto;white-space:pre-wrap;word-break:break-all;border-radius:6px;font-size:.8rem}
button{font-size:1.1rem;padding:.7rem 1.2rem;margin:.4rem .4rem .4rem 0;cursor:pointer}
.ok{color:#0a7a2f}.bad{color:#b00020}.box{border:2px solid #444;border-radius:8px;padding:1rem;margin:1rem 0}
</style></head><body>
<h1>Sign the URnetwork wallet consent</h1>
<p>This approves that earnings of every provider client of your URnetwork network are paid to your wallet
<b id="addr"></b>. Your Talisman extension does the signing. Your seed phrase is never typed or shown here.</p>
<div class="box"><b>Check before you sign:</b>
<ol>
<li>The address above is your wallet.</li>
<li>The text below starts with <code>Approve URnetwork network wallet mapping</code>.</li>
<li>Talisman will show the same text when it asks you to sign.</li>
</ol></div>
<p>Time left on this message: <b id="left"></b></p>
<button id="connect">1. Connect Talisman</button>
<button id="sign" disabled>2. Sign</button>
<p id="status"></p>
<h3>The exact message</h3>
<pre id="msg"></pre>
<script>
const ADDRESS = __ADDRESS__;
const MESSAGE = __MESSAGE__;
const MESSAGE_HEX = __HEX__;
const DEADLINE = __DEADLINE__;
document.getElementById('addr').textContent = ADDRESS;
document.getElementById('msg').textContent = MESSAGE;
const status = (t, cls) => { const s = document.getElementById('status'); s.textContent = t; s.className = cls || ''; };
setInterval(() => {
  const left = Math.max(0, Math.round(DEADLINE - Date.now() / 1000));
  document.getElementById('left').textContent = Math.floor(left / 60) + ':' + String(left % 60).padStart(2, '0');
}, 500);
let ext = null, account = null;
document.getElementById('connect').onclick = async () => {
  try {
    const inj = window.injectedWeb3 || {};
    const provider = inj.talisman || inj['polkadot-js'] || Object.values(inj)[0];
    if (!provider) { status('No wallet extension found. Open this page in the browser that has Talisman, and allow Talisman to run on this site.', 'bad'); return; }
    ext = await provider.enable('URnetwork wallet consent');
    const accounts = await ext.accounts.get();
    account = accounts.find(a => a.address === ADDRESS);
    if (!account) {
      status('Talisman does not list ' + ADDRESS + '. Accounts it shows: ' + accounts.map(a => a.address).join(', ') + '. Make sure this account is enabled for sites in Talisman.', 'bad');
      return;
    }
    status('Connected to ' + (account.name || 'your account') + '. Now click Sign.', 'ok');
    document.getElementById('sign').disabled = false;
  } catch (e) { status('Connect failed: ' + e, 'bad'); }
};
document.getElementById('sign').onclick = async () => {
  try {
    const result = await ext.signer.signRaw({ address: account.address, data: MESSAGE_HEX, type: 'bytes' });
    const r = await fetch('/signature', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ signature: result.signature }) });
    if (!r.ok) { status('The signature was made but not saved: ' + await r.text(), 'bad'); return; }
    status('Signed and saved. You can close this tab and tell Claude it is done.', 'ok');
    document.getElementById('sign').disabled = true;
  } catch (e) { status('Signing failed or was cancelled: ' + e, 'bad'); }
};
</script></body></html>
"""

def js(value):
    return json.dumps(value).replace("</", "<\\/")

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        if self.path not in ("/", "/index.html"):
            self.send_error(404)
            return
        body = (PAGE.replace("__ADDRESS__", js(address)).replace("__MESSAGE__", js(message))
                .replace("__HEX__", js(message_hex)).replace("__DEADLINE__", str(int(deadline)))).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        host = self.headers.get("Host", "")
        origin = self.headers.get("Origin", "")
        if self.path != "/signature" or host.split(":")[0] not in ("127.0.0.1", "localhost") or (
                origin and origin.split("//")[-1].split(":")[0] not in ("127.0.0.1", "localhost")):
            self.send_error(403)
            return
        if time.time() > deadline:
            self.send_error(410, "the message expired")
            return
        length = int(self.headers.get("Content-Length", "0"))
        try:
            signature = json.loads(self.rfile.read(min(length, 4096)))["signature"]
        except Exception:
            self.send_error(400, "bad request")
            return
        text = signature[2:] if signature.startswith("0x") else signature
        if len(text) != 128 or any(c not in "0123456789abcdefABCDEF" for c in text):
            self.send_error(400, "not a 64-byte hex signature")
            return
        fd = os.open(out_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            f.write("0x" + text.lower() + "\n")
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")
        threading.Thread(target=server.shutdown, daemon=True).start()

server = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
threading.Timer(7 * 60, server.shutdown).start()
print(f"open http://127.0.0.1:{port}/ in the browser that has Talisman", flush=True)
server.serve_forever()
print("done", flush=True)
