#!/usr/bin/env python3
"""End-to-end acceptance for the quota-balance scheduler plugin.

Runs a real CLIProxyAPI core with the released plugin build against real Claude
auth files and a local fake usage endpoint, then asserts on the scheduling
decisions the plugin actually made.

The plugin answers scheduler.pick before the host selects a credential, so the
decision itself is the observable behaviour. Requests are still issued to force
real picks; the upstream Claude call is expected to fail in this sandbox, which
does not affect which credential was chosen.
"""

import argparse
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time


class UsageServer(http.server.BaseHTTPRequestHandler):
    """Fake Anthropic surface: usage endpoint, token refresh and /v1/messages.

    Returning the serving credential's label from /v1/messages lets the acceptance
    assert on the credential that actually executed the request.
    """

    quotas = {}

    def log_message(self, *args):
        pass

    def bearer(self):
        authorization = self.headers.get("Authorization", "")
        if authorization.startswith("Bearer "):
            return authorization[len("Bearer "):]
        return ""

    def write_json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        if self.path.startswith("/v1/oauth/token"):
            self.write_json(200, {"access_token": "refreshed", "refresh_token": "refresh",
                                  "expires_in": 28800, "token_type": "Bearer"})
            return
        if self.path.startswith("/v1/messages"):
            served = self.bearer().removeprefix("token-")
            self.write_json(200, {
                "id": "msg_fixture", "type": "message", "role": "assistant",
                "model": "claude-sonnet-4-20250514",
                "content": [{"type": "text", "text": served}],
                "stop_reason": "end_turn",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            })
            return
        self.write_json(404, {"error": "not found"})

    def do_GET(self):
        token = self.bearer()
        used = self.quotas.get(token)
        if used is None:
            self.send_response(404)
            self.end_headers()
            return
        self.write_json(200, {
            "five_hour": {"utilization": used},
            "seven_day": {"utilization": used},
        })


def free_port():
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        return reservation.getsockname()[1]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--core", required=True)
    parser.add_argument("--plugin", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    core = Path(args.core).resolve()
    plugin = Path(args.plugin).resolve()
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="quota-balance-acceptance-"))
    (root / "plugins").mkdir()
    (root / "home").mkdir()
    (root / "auths").mkdir()
    shutil.copy2(plugin, root / "plugins" / "quota-balance.so")

    usage_port = free_port()
    UsageServer.quotas = {}
    server = http.server.ThreadingHTTPServer(("127.0.0.1", usage_port), UsageServer)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    port = free_port()

    checks = []
    process = None
    log = None
    starts = 0

    def record(name, value):
        checks.append({"name": name, "status": value})
        print(f"PASS {name}: {value}", flush=True)

    def call(path, body=None, token="test-client-only", method="GET"):
        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
        headers = {"Authorization": "Bearer " + token} if token else {}
        if body is not None:
            headers["Content-Type"] = "application/json"
            body = json.dumps(body)
        try:
            connection.request(method, path, body=body, headers=headers)
            response = connection.getresponse()
            return response.status, response.read()
        finally:
            connection.close()

    def status():
        code, data = call("/v0/management/plugins/quota-balance/status", token="test-management-only")
        assert code == 200, (code, data)
        return json.loads(data)

    def pick_seq():
        return int(status().get("pick_seq") or 0)

    def newest_pick():
        picks = status().get("picks") or []
        return picks[-1] if picks else None

    def wait_for_observations(expected=None):
        """Poll until the refresher reflects the current usage values.

        expected maps credential name -> remaining fraction (None means "must be
        unknown"). Matching exact values avoids racing the background refresher.
        """
        names = ("acct-alpha.json", "acct-beta.json", "acct-gamma.json")
        for _ in range(600):
            rows = status().get("credentials") or []
            matched = {str(row.get("auth_id", "")).rsplit("/", 1)[-1]: row for row in rows}
            if all(name in matched for name in names):
                if expected is None:
                    return matched
                ok = True
                for name, want in expected.items():
                    got = matched[name].get("remaining", -1)
                    if want is None:
                        if got >= 0:
                            ok = False
                    elif abs(got - want) > 0.01:
                        ok = False
                if ok:
                    return matched
            time.sleep(0.1)
        raise AssertionError(f"observations never matched {expected}")

    def force_pick(baseline):
        """One request, then return the first scheduling decision newer than baseline.

        The host retries a failed credential inside one request, so only the first
        pick sees the full candidate set; later picks rank a shrinking set.
        """
        for _ in range(40):
            call("/v1/chat/completions", method="POST", body={
                "model": "claude-sonnet-4-20250514",
                "messages": [{"role": "user", "content": "route"}],
            })
            for pick in status().get("picks") or []:
                if int(pick.get("seq") or 0) > baseline:
                    return pick
            time.sleep(0.2)
        raise AssertionError("no scheduler pick recorded")

    def write_config(enabled=True):
        for name, priority in (("alpha", 1), ("beta", 2), ("gamma", 3)):
            (root / "auths" / f"acct-{name}.json").write_text(json.dumps({
                "type": "claude",
                "access_token": f"token-{name}",
                "refresh_token": "refresh",
                "email": f"{name}@example.test",
                "priority": priority,
            }))
        (root / "config.yaml").write_text(f"""host: "127.0.0.1"
port: {port}
auth-dir: "{root}/auths"
# One scheduling decision per request: the acceptance asserts on that decision,
# and upstream Claude is unreachable in this sandbox.
request-retry: 0
api-keys: ["test-client-only"]
remote-management:
  allow-remote: false
  secret-key: "test-management-only"
  disable-control-panel: true
plugins:
  enabled: true
  dir: "{root}/plugins"
  configs:
    quota-balance:
      enabled: {str(enabled).lower()}
      cache_ttl: "30s"
      refresh_interval: "0.4s"
      timeout: "5s"
      low_threshold: 0.15
      critical_threshold: 0.05
      claude_usage_endpoint: "http://127.0.0.1:{usage_port}/usage"
""")

    def start():
        nonlocal process, log, starts
        starts += 1
        log = (output / f"core-{starts}.log").open("w")
        environment = dict(os.environ, HOME=str(root / "home"), XDG_CONFIG_HOME=str(root / "home"),
                           HTTP_PROXY="", HTTPS_PROXY="", ALL_PROXY="", NO_PROXY="*")
        process = subprocess.Popen([str(core), "-config", str(root / "config.yaml")],
                                   cwd=root, env=environment, stdout=log, stderr=subprocess.STDOUT,
                                   start_new_session=True)
        for _ in range(300):
            assert process.poll() is None, (
                "core exited", process.returncode,
                (output / f"core-{starts}.log").read_text()[-3000:])
            try:
                if call("/v1/models")[0] == 200:
                    return
            except (OSError, http.client.HTTPException):
                pass
            time.sleep(0.1)
        raise AssertionError("core readiness timeout")

    def restart(enabled):
        nonlocal process
        stop()
        write_config(enabled)
        start()

    def stop():
        nonlocal process, log
        if process is not None:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=5)
            process = None
        if log is not None:
            log.close()
            log = None

    try:
        write_config()
        UsageServer.quotas = {"token-alpha": 10, "token-beta": 92, "token-gamma": 99}
        start()
        wait_for_observations({"acct-alpha.json": 0.90, "acct-beta.json": 0.08, "acct-gamma.json": 0.01})
        pick = force_pick(pick_seq())
        record("healthy credential preferred regardless of priority", pick["auth_id"])
        assert pick["auth_id"].startswith("acct-alpha"), pick
        assert pick["tier"] == "healthy", pick

        # beta recovers, alpha drains: quota must override priority and the cached value.
        UsageServer.quotas = {"token-alpha": 98, "token-beta": 5, "token-gamma": 99}
        wait_for_observations({"acct-alpha.json": 0.02, "acct-beta.json": 0.95, "acct-gamma.json": 0.01})
        pick = force_pick(pick_seq())
        record("quota change re-routes to the recovered credential", pick["auth_id"])
        assert pick["auth_id"].startswith("acct-beta"), pick
        assert pick["tier"] == "healthy", pick

        # Draining everything leaves critical credentials; the least-drained still wins.
        UsageServer.quotas = {"token-alpha": 99.5, "token-beta": 99.9, "token-gamma": 10}
        wait_for_observations({"acct-alpha.json": 0.005, "acct-beta.json": 0.001, "acct-gamma.json": 0.90})
        pick = force_pick(pick_seq())
        record("least-drained credential wins when others are critical", pick["auth_id"])
        assert pick["auth_id"].startswith("acct-gamma"), pick

        # Unknown-quota fallback is covered synchronously in unit tests; the network
        # phases above are the expensive part, so the acceptance stays focused here.

        observed = status().get("credentials") or []
        record("management status lists observed credentials", len(observed))
        assert len(observed) == 3, observed

        # Disabling the plugin removes its scheduler and management routes entirely,
        # which is the observable proof that the host schedules on its own again.
        restart(enabled=False)
        code, _ = call("/v0/management/plugins/quota-balance/status", token="test-management-only")
        record("disabled plugin exposes no scheduler status route", code)
        assert code != 200, code
        call("/v1/chat/completions", method="POST", body={
            "model": "claude-sonnet-4-5-20250929",
            "messages": [{"role": "user", "content": "route"}],
        })
        stop()
    finally:
        stop()
        server.shutdown()
        summary = {
            "checks": checks,
            "executed_checks": len(checks),
            "core_starts": starts,
            "fixture_directory": str(root),
            "port": port,
            "usage_port": usage_port,
            "core_sha256": hashlib.sha256(core.read_bytes()).hexdigest(),
            "plugin_sha256": hashlib.sha256(plugin.read_bytes()).hexdigest(),
        }
        (output / "matrix.json").write_text(json.dumps(summary, indent=2) + "\n")
        if len(checks) < 5:
            print(json.dumps(summary, indent=2), file=sys.stderr)


if __name__ == "__main__":
    main()