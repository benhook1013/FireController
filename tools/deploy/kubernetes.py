#!/usr/bin/env python3
"""Explicit single-writer rollout of an already published image, with bounded drain."""
import argparse
import uuid
import json
from pathlib import Path
import re
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_DRAIN_SECONDS = 600


class OperatorUnavailable(RuntimeError):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def wait_for_drain(read_status, timeout, sleep=time.sleep, clock=time.monotonic):
    deadline = clock() + timeout
    while True:
        status = read_status()
        if status.get("draining") is not True or type(status.get("active_dispatches")) is not int or status["active_dispatches"] < 0:
            raise ValueError("controller did not confirm its deployment drain")
        if status["active_dispatches"] == 0:
            return False
        remaining = deadline - clock()
        if remaining <= 0:
            return True
        sleep(min(2, remaining))


def wait_for_recovery(read_status, expected_instance, timeout=30, sleep=time.sleep, clock=time.monotonic):
    deadline = clock() + timeout
    while True:
        try:
            instance, drain = read_status()
            if instance.get("instance_id") != expected_instance:
                raise ValueError("replacement has an unexpected instance identity")
            if drain.get("draining") is False:
                return
        except OperatorUnavailable:
            pass
        remaining = deadline - clock()
        if remaining <= 0:
            raise RuntimeError("replacement did not recover HTTPS and open admission within the verification window")
        sleep(min(1, remaining))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--image", required=True, help="Registry image with immutable sha256 digest")
    p.add_argument("--source-revision", required=True, help="Full source commit recorded by the publication run")
    p.add_argument("--url", required=True, help="Existing controller HTTPS origin (loopback HTTP also accepted)")
    p.add_argument("--instance-id", required=True)
    p.add_argument("--credential-file", required=True, type=Path, help="Private global administrator credential on this host")
    p.add_argument("--namespace", default="firecontroller")
    p.add_argument("--deployment", default="firecontroller")
    p.add_argument("--container", default="controller")
    p.add_argument("--context")
    p.add_argument("--drain-seconds", type=int, default=DEFAULT_DRAIN_SECONDS)
    p.add_argument("--kubectl", nargs="+", default=["kubectl"], help="Command prefix, for example --kubectl k3s kubectl")
    args = p.parse_args()
    if not re.fullmatch(r"[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}", args.image) or not re.fullmatch(r"[0-9a-f]{40}", args.source_revision) or not 0 <= args.drain_seconds <= 3600:
        p.error("use an immutable image digest and a drain window from 0 through 3600 seconds")
    origin = urllib.parse.urlsplit(args.url)
    if origin.username or origin.password or origin.query or origin.fragment or origin.path not in ("", "/") or not origin.hostname or origin.scheme != "https" and not (origin.scheme == "http" and origin.hostname in ("127.0.0.1", "::1", "localhost")):
        p.error("url must be an HTTPS origin or loopback HTTP origin")
    # Credentials are neither command arguments nor printed diagnostic material.
    credential = args.credential_file.read_text().strip()
    if not credential or len(credential) > 256 or any(c.isspace() for c in credential):
        p.error("invalid administrator credential file")
    opener = urllib.request.build_opener(NoRedirect)

    def api(method, path):
        request = urllib.request.Request(args.url.rstrip("/") + path, method=method,
            data=b"{}" if method == "POST" else None,
            headers={"Authorization": "Bearer " + credential, "X-FC-Instance": args.instance_id, "Content-Type": "application/json"})
        try:
            with opener.open(request, timeout=5) as response:
                body = response.read(16385)
                if len(body) > 16384:
                    raise ValueError("oversized operator response")
                return json.loads(body)
        except urllib.error.HTTPError as exc:
            failure = OperatorUnavailable if exc.code in (502, 503, 504) else RuntimeError
            raise failure("operator request failed with HTTP " + str(exc.code)) from None
        except (urllib.error.URLError, TimeoutError):
            raise OperatorUnavailable("operator connection failed; inspect the same deployment before retrying") from None

    k = args.kubectl + (["--context", args.context] if args.context else []) + ["-n", args.namespace]

    def command(arguments, timeout=30):
        result = subprocess.run(k + arguments, capture_output=True, text=True, timeout=timeout)
        if result.returncode:
            raise RuntimeError("Kubernetes operation failed; inspect deployment status before retrying")
        return result.stdout

    deployment = json.loads(command(["get", "deployment", args.deployment, "-o", "json"]))
    if deployment["spec"].get("replicas") != 1 or deployment["spec"].get("strategy", {}).get("type") != "Recreate":
        p.error("deployment must have one replica and Recreate strategy")
    containers = deployment["spec"]["template"]["spec"]["containers"]
    if sum(c["name"] == args.container for c in containers) != 1:
        p.error("selected controller container is unavailable")
    if api("GET", "/api/v1/instance").get("instance_id") != args.instance_id:
        p.error("unexpected service instance")
    api("GET", "/api/v1/operator/deployment-drain")  # Refuse unsupported old servers before changing anything.
    # Pull and start the image on the serving node before touching admission.
    # This pod has no live data, credentials, provider mounts or service token.
    labels = deployment["spec"]["selector"]["matchLabels"]
    selector = ",".join(key + "=" + value for key, value in labels.items())
    pods = json.loads(command(["get", "pods", "-l", selector, "-o", "json"]))["items"]
    nodes = {pod["spec"].get("nodeName") for pod in pods if pod.get("status", {}).get("phase") == "Running"}
    if len(nodes) != 1 or None in nodes:
        p.error("select a deployment with one current serving node")
    name = "fc-image-check-" + uuid.uuid4().hex[:12]
    spec = {
        "apiVersion": "v1", "kind": "Pod",
        "metadata": {"name": name},
        "spec": {
            "nodeName": next(iter(nodes)), "restartPolicy": "Never",
            "automountServiceAccountToken": False, "activeDeadlineSeconds": 90,
            "terminationGracePeriodSeconds": 5,
            "imagePullSecrets": deployment["spec"]["template"]["spec"].get("imagePullSecrets", []),
            "containers": [{
                "name": "check", "image": args.image,
                "command": ["/bin/sh", "-ec", "git --version >/dev/null; /fcd --help >/dev/null 2>&1"],
                "securityContext": {"runAsUser": 65532, "runAsGroup": 65532, "runAsNonRoot": True,
                    "readOnlyRootFilesystem": True, "allowPrivilegeEscalation": False,
                    "capabilities": {"drop": ["ALL"]}, "seccompProfile": {"type": "RuntimeDefault"}},
                "resources": {"requests": {"cpu": "10m", "memory": "32Mi"}, "limits": {"cpu": "1", "memory": "128Mi"}},
            }],
        },
    }
    try:
        created = subprocess.run(k + ["create", "-f", "-"], input=json.dumps(spec), text=True, capture_output=True, timeout=30)
        if created.returncode:
            raise RuntimeError("image preflight could not start; live admission is unchanged")
        command(["wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/" + name, "--timeout=95s"], timeout=100)
    finally:
        command(["delete", "pod", name, "--ignore-not-found", "--timeout=30s"], timeout=35)
    began, replacing = False, False
    try:
        began = True  # A lost POST response may still have started the drain.
        api("POST", "/api/v1/operator/deployment-drain")
        forced = wait_for_drain(lambda: api("GET", "/api/v1/operator/deployment-drain"), args.drain_seconds)
        # Prevent changing a deployment that another operator changed while we waited.
        deployment["spec"]["template"]["spec"]["terminationGracePeriodSeconds"] = 90
        for c in containers:
            if c["name"] == args.container:
                c["image"] = args.image
                c["imagePullPolicy"] = "IfNotPresent"
        annotations = deployment["spec"]["template"].setdefault("metadata", {}).setdefault("annotations", {})
        annotations["firecontroller.dev/source-commit"] = args.source_revision
        annotations["firecontroller.dev/image-reference"] = args.image
        annotations["firecontroller.dev/rollout-id"] = uuid.uuid4().hex
        annotations.pop("firecontroller.dev/binary-sha256", None)  # Old locally built binary provenance cannot describe this image.
        replacing = True  # A replace with a lost outcome must not resume old admission.
        result = subprocess.run(k + ["replace", "-f", "-"], input=json.dumps(deployment), text=True, capture_output=True, timeout=30)
        if result.returncode:
            raise RuntimeError("replacement refused; inspect this deployment before cancelling drain or retrying")
        command(["rollout", "status", "deployment/" + args.deployment, "--timeout=120s"], timeout=130)
        current = json.loads(command(["get", "deployment", args.deployment, "-o", "json"]))
        if next(c["image"] for c in current["spec"]["template"]["spec"]["containers"] if c["name"] == args.container) != args.image:
            raise RuntimeError("selected image changed during rollout")
        wait_for_recovery(lambda: (api("GET", "/api/v1/instance"), api("GET", "/api/v1/operator/deployment-drain")), args.instance_id)
        print(json.dumps({"state": "deployed", "image": args.image, "drain_deadline_reached": forced}))
    finally:
        if began and not replacing:
            api("DELETE", "/api/v1/operator/deployment-drain")


if __name__ == "__main__":
    main()
