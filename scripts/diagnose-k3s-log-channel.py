#!/usr/bin/env python3
"""Read-only host diagnostics. Prints selected proxy/network data, never tokens or kubeconfigs."""
import json
from pathlib import Path
import re
import subprocess
from urllib.parse import urlsplit


def output(args):
    return subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL, timeout=15).strip()


def main():
    pid = output(["systemctl", "show", "k3s", "--property=MainPID", "--value"])
    root = Path("/proc") / pid
    raw = (root / "environ").read_bytes().split(b"\0")
    proxies = {}
    for item in raw:
        key, sep, value = item.decode(errors="replace").partition("=")
        if not sep or key.lower() not in {"http_proxy", "https_proxy", "all_proxy", "no_proxy"}:
            continue
        if key.lower() == "no_proxy":
            proxies[key] = value
        else:
            parsed = urlsplit(value)
            proxies[key] = {"scheme": parsed.scheme, "host": parsed.hostname, "port": parsed.port}
    args = (root / "cmdline").read_bytes().decode(errors="replace").split("\0")
    flags = []
    for index, arg in enumerate(args):
        if arg.split("=", 1)[0] in {"--egress-selector-mode", "--node-ip", "--bind-address", "--advertise-address"}:
            flags.append(arg if "=" in arg else arg + "=" + args[index + 1])
    logs = output(["journalctl", "-u", "k3s", "--since", "10 minutes ago", "-n", "2000", "--no-pager", "-o", "cat"])
    needles = ("EOF", "remotedialer", "proxy error", "error dialing", "x509:", "bad certificate", "connection refused", "no route to host")
    excerpts = []
    for line in logs.splitlines():
        if not any(word.lower() in line.lower() for word in needles):
            continue
        line = re.sub(r'https?://[^\s"\\]+', '<url>', line)
        line = re.sub(r'(?i)(token|password|secret|authorization)([=: ]+)\S+', r'\1\2<redacted>', line)
        excerpts.append(line[:1200])
    print(json.dumps({"pid": int(pid), "proxyEnvironment": proxies, "networkFlags": flags,
                      "recentNetworkErrors": excerpts[-30:], "changesPerformed": False}, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        raise SystemExit("diagnostic failed: " + str(error))
