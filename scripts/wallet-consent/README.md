# Signing the network wallet consent with a browser wallet

Release settlement pays only a wallet that its coldkey has consented to. The consent is one signature that covers every provider client of your network, current and future. This folder signs it with a browser wallet extension (Talisman, or any extension that exposes the Polkadot `signRaw` API), so the seed phrase never leaves the extension.

Run it on your own computer, never on a fleet node. A fleet node needs nothing.

## Steps

1. Save a network login on this computer: `provider auth` (it asks for an auth code from the app, or your email and password).
2. Check what is set today: `provider wallet status`.
3. Run `./run_signing.sh <your coldkey ss58 address>`. `provider` must be on your PATH, or set `TOOL=/path/to/provider`.
4. Open the address it prints (`http://127.0.0.1:8765/`) in the browser that has your wallet extension, and unlock the extension.
5. Click **1. Connect Talisman**, approve in the extension, then click **2. Sign** and approve. The extension shows the message; it starts with `Approve URnetwork network wallet mapping`.
6. The script submits the signature, checks the server's receipt against the hash it computed itself, and prints the stored epochs.
7. Check: `provider wallet status` shows `network consent ... in effect now` (or `starts in N epoch(s)`).

The statement is checked against your login, the coldkey you named and the epoch route before the page is shown. The message is valid for 5 minutes. If it expires, run the script again.

## What the page is

`sign_server.py` serves one page on `127.0.0.1` only. It hands the message to the extension and writes the returned signature (a public value) to a private temporary file. It never sees a seed phrase or key, and it rejects requests that are not from the local page.

## Replacing a consent

A later consent must start at a later epoch than the one before it. Use `--wallet-from-epoch` and `--wallet-through-epoch` with `provider wallet challenge` and `provider wallet set` for an explicit interval; the default is the next epoch through the full 65,536-epoch span. A mistaken interval cannot be undone, so the tool refuses odd intervals unless `URNETWORK_WALLET_ALLOW_ODD_INTERVAL=1` is set.
