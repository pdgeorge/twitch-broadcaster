#!/usr/bin/env python3
"""
Lightweight helper to generate a Twitch authorization code with the chat scopes
and exchange it for tokens. Run this outside Docker, visit the printed URL,
approve the scopes, and paste the resulting refresh token into your .env.
"""

import argparse
import http.server
import json
import secrets
import sys
import threading
import urllib.parse
import urllib.request

AUTHORIZE_URL = "https://id.twitch.tv/oauth2/authorize"
TOKEN_URL = "https://id.twitch.tv/oauth2/token"


def build_authorize_url(client_id: str, redirect_uri: str, scopes: list[str], state: str) -> str:
    params = {
        "response_type": "code",
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "scope": " ".join(scopes),
        "state": state,
        "force_verify": "true",
    }
    return f"{AUTHORIZE_URL}?{urllib.parse.urlencode(params)}"


class CallbackHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path != "/callback":
            self.send_error(404)
            return

        query = urllib.parse.parse_qs(parsed.query)
        code = query.get("code", [None])[0]
        state = query.get("state", [None])[0]

        if code is None or state is None:
            self.send_error(400, "Missing code or state")
            return

        if state != self.server.expected_state:  # type: ignore[attr-defined]
            self.send_error(400, "State mismatch")
            return

        try:
            token_response = exchange_code_for_tokens(
                self.server.client_id,  # type: ignore[attr-defined]
                self.server.client_secret,  # type: ignore[attr-defined]
                self.server.redirect_uri,  # type: ignore[attr-defined]
                code,
            )
        except Exception as exc:  # noqa: BLE001
            self.send_error(500, f"Token exchange failed: {exc}")
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.end_headers()
        self.wfile.write(
            b"<h1>Authorization complete</h1><p>You can close this tab.</p>"
        )

        print("\nAccess token:\n", token_response.get("access_token"))
        print("\nRefresh token (store in .env as REFRESH_TOKEN):\n", token_response.get("refresh_token"))
        print("\nToken scopes:\n", token_response.get("scope"))
        self.server.stop_event.set()  # type: ignore[attr-defined]

    def log_message(self, fmt, *args):  # noqa: D401
        """Silence default logging."""
        return


def exchange_code_for_tokens(client_id: str, client_secret: str, redirect_uri: str, code: str) -> dict:
    data = urllib.parse.urlencode(
        {
            "client_id": client_id,
            "client_secret": client_secret,
            "code": code,
            "grant_type": "authorization_code",
            "redirect_uri": redirect_uri,
        }
    ).encode()

    req = urllib.request.Request(TOKEN_URL, data=data, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read()
    return json.loads(body)


def main():
    parser = argparse.ArgumentParser(description="Twitch OAuth helper")
    parser.add_argument("--client-id", required=True, help="Twitch application client ID")
    parser.add_argument("--client-secret", required=True, help="Twitch application client secret")
    parser.add_argument(
        "--redirect-uri",
        default="http://localhost:17563/callback",
        help="Redirect URI registered in your Twitch app (default: %(default)s)",
    )
    parser.add_argument(
        "--scopes",
        nargs="*",
        default=["chat:edit", "chat:read"],
        help="Space-separated scopes to request (default: chat:edit chat:read)",
    )
    args = parser.parse_args()

    state = secrets.token_urlsafe(16)
    authorize_url = build_authorize_url(args.client_id, args.redirect_uri, args.scopes, state)
    print("Open this URL in your browser and authorize as the broadcaster:\n")
    print(authorize_url)

    stop_event = threading.Event()
    server = http.server.HTTPServer(("", urllib.parse.urlparse(args.redirect_uri).port or 80), CallbackHandler)
    server.expected_state = state  # type: ignore[attr-defined]
    server.client_id = args.client_id  # type: ignore[attr-defined]
    server.client_secret = args.client_secret  # type: ignore[attr-defined]
    server.redirect_uri = args.redirect_uri  # type: ignore[attr-defined]
    server.stop_event = stop_event  # type: ignore[attr-defined]

    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    print("\nWaiting for the OAuth callback on", args.redirect_uri)
    try:
        stop_event.wait()
    except KeyboardInterrupt:
        print("\nCancelled by user")
    finally:
        server.shutdown()
        thread.join(timeout=2)


if __name__ == "__main__":
    sys.exit(main())
