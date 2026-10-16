#!/usr/bin/env python3
"""fxTunnel Go runtime smoke. Network-isolated; never uses real credentials."""
import argparse
import json
import subprocess
import time
import uuid


def docker(*args, check=True):
    return subprocess.run(["docker", *args], check=check, text=True, capture_output=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", default="fxtunnel-receipts:test")
    args = parser.parse_args()
    name = "fxtunnel-receipts-smoke-" + uuid.uuid4().hex[:12]
    env = {
        "YOOKASSA_SHOP_ID": "smoke-shop",
        "YOOKASSA_API_KEY": "not-a-real-key",
        "MOY_NALOG_AUTH_METHOD": "refresh",
        "MOY_NALOG_REFRESH_TOKEN": "not-a-real-token",
        "DEVICE_ID": "smoke-device",
        "MOY_NALOG_RECEIPT_INN": "123456789012",
        # No financial work is due. The network is disabled as a second boundary.
        "SYNC_START_DATE": "2099-01-01T00:00:00Z",
        "DELIVERY_MODE": "disabled",
    }
    command = [
        "run", "-d", "--rm", "--name", name, "--network", "none",
        "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
        "--tmpfs", "/data:rw,uid=10001,gid=10001,mode=0700",
        "--tmpfs", "/tmp:rw,mode=1777",
    ]
    for key, value in env.items():
        command += ["-e", key + "=" + value]
    try:
        docker(*command, args.image)
        health = None
        for _ in range(40):
            response = docker("exec", name, "wget", "-qO-", "http://127.0.0.1:8099/readyz", check=False)
            if response.returncode == 0:
                health = json.loads(response.stdout)
                break
            time.sleep(0.2)
        assert health and health["ready"], "runtime did not become ready"
        assert health["counts"] == {}, "unexpected financial jobs in isolated smoke"
        status = json.loads(docker("exec", name, "receipts", "status").stdout)
        assert status["counts"] == {} and status["pending"] == []
        docker("exec", name, "receipts", "backup", "--out", "/data/smoke-backup.db")
        snapshot = json.loads(docker("exec", name, "receipts", "status", "--db", "/data/smoke-backup.db").stdout)
        assert snapshot["counts"] == {}, "backup did not preserve state"
        other = docker("exec", name, "receipts", "once", check=False)
        assert other.returncode != 0 and "another receipt writer" in other.stdout, "second writer was not blocked"
        user = docker("inspect", "--format", "{{.Config.User}}", name).stdout.strip()
        assert user == "receipts:receipts", "runtime is not non-root"
        print(json.dumps({"ready": True, "read_only_rootfs": True, "external_network": False,
                          "user": user, "backup": "ok", "second_writer": "blocked", "tax_writes": 0}))
    finally:
        docker("stop", "-t", "5", name, check=False)


if __name__ == "__main__":
    main()
