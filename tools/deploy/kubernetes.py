#!/usr/bin/env python3
"""Explicit single-writer rollout of an already published image, with bounded drain."""
import argparse
import uuid
import json
import os
from pathlib import Path
import re
import stat
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



def rollout_custody_fds():
    """Validate the deployment, generation and backup locks inherited from the host."""
    raw = os.environ.get("FC_ROLLOUT_CUSTODY_FDS")
    if raw is None:
        return ()
    if os.name != "posix" or not re.fullmatch(r"[0-9]{1,10}(?:,[0-9]{1,10}){2}", raw):
        raise ValueError("invalid inherited rollout custody descriptors")
    fds = tuple(int(part) for part in raw.split(","))
    if min(fds) < 3 or len(set(fds)) != 3:
        raise ValueError("invalid inherited rollout custody descriptors")
    for fd in fds:
        try:
            mode = os.fstat(fd).st_mode
        except (OSError, OverflowError):
            raise ValueError("unavailable inherited rollout custody descriptor") from None
        if not stat.S_ISREG(mode):
            raise ValueError("inherited rollout custody descriptor is not a regular file")
    return fds


def artifact_lease_options(custody_fds=()):
    """Preserve an optional local managed-runner lease; never infer remote custody."""
    raw = os.environ.get("FC_ARTIFACT_LEASE_FD")
    if raw is None:
        return {"pass_fds": custody_fds} if custody_fds else {}
    if os.name != "posix" or not re.fullmatch(r"[0-9]{1,10}", raw):
        raise ValueError("invalid inherited artifact lease")
    fd = int(raw)
    if fd < 3:
        raise ValueError("invalid inherited artifact lease")
    try:
        os.fstat(fd)
    except (OSError, OverflowError):
        raise ValueError("unavailable inherited artifact lease") from None
    return {"pass_fds": tuple(dict.fromkeys((*custody_fds, fd)))}


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


def require_crashloop(api, deployment, pods, container):
    """Prove the old single writer is down before a crash-only replacement."""
    try:
        api("GET", "/api/v1/instance")
    except OperatorUnavailable:
        pass
    else:
        raise RuntimeError("Controller responds; use the normal drained rollout")
    if deployment.get("status", {}).get("readyReplicas", 0) != 0 or len(pods) != 1:
        raise RuntimeError("crash recovery requires exactly one unready Controller pod")
    pod = pods[0]
    owner = [reference.get("uid") for reference in pod.get("metadata", {}).get("ownerReferences", [])
             if reference.get("kind") == "ReplicaSet" and reference.get("controller") is True]
    statuses = [status for status in pod.get("status", {}).get("containerStatuses", [])
                if status.get("name") == container]
    if pod.get("metadata", {}).get("deletionTimestamp") or len(owner) != 1 or not owner[0] or len(statuses) != 1:
        raise RuntimeError("crash recovery could not identify the Controller container")
    status = statuses[0]
    if (status.get("ready") is not False or status.get("restartCount", 0) < 2
            or status.get("state", {}).get("waiting", {}).get("reason") != "CrashLoopBackOff"
            or "terminated" not in status.get("lastState", {})):
        raise RuntimeError("crash recovery requires a previously terminated Controller in CrashLoopBackOff")


def retention_args(current, mode):
    """Change only the selected daemon flag, preserving all other arguments."""
    if mode is None:
        return current
    if not isinstance(current, list) or not all(isinstance(value, str) for value in current):
        raise ValueError("invalid existing controller arguments")
    if mode not in ("dry-run", "enabled"):
        raise ValueError("invalid Controller retention mode")
    args = [value for value in current
            if value.split("=", 1)[0] not in ("--retention-dry-run", "-retention-dry-run")]
    return args + (["--retention-dry-run"] if mode == "dry-run" else [])


def remote_mcp_args(current, mode, origin):
    """Validate the existing selector and change only the explicit MCP opt-in."""
    if mode is None:
        return current
    if not isinstance(current, list) or not all(isinstance(value, str) for value in current):
        raise ValueError("invalid existing controller arguments")
    if mode not in ("enable", "disable"):
        raise ValueError("invalid remote MCP mode")

    def require_https(value):
        parsed = urllib.parse.urlsplit(value)
        # Accessing port also rejects malformed or out-of-range ports.
        _ = parsed.port
        if (parsed.scheme != "https" or not parsed.hostname or parsed.username is not None
                or parsed.password is not None or parsed.query or parsed.fragment
                or parsed.path not in ("", "/") or any(c.isspace() for c in value)
                or "?" in value or "#" in value or parsed.netloc.endswith(":")):
            raise ValueError("remote MCP requires a canonical HTTPS origin")

    if mode == "enable":
        require_https(origin)
    result, found, index = [], False, 0
    while index < len(current):
        value = current[index]
        flag, equals, inline = value.partition("=")
        if flag not in ("--remote-mcp-origin", "-remote-mcp-origin"):
            result.append(value)
            index += 1
            continue
        if found:
            raise ValueError("duplicate existing remote MCP origin selector")
        found = True
        index += 1
        if not equals:
            if index == len(current):
                raise ValueError("missing existing remote MCP origin value")
            inline = current[index]
            index += 1
        require_https(inline)
    if mode == "enable":
        if "--" in result:
            raise ValueError("cannot enable remote MCP after an argument terminator")
        result.extend(["--remote-mcp-origin", origin.rstrip("/")])
    return result


def main():
    artifact_lease_options()  # Refuse invalid enrollment before any rollout action.
    custody_fds = rollout_custody_fds()
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
    p.add_argument("--recover-crashloop", action="store_true", help="replace a verified offline, crash-looping Controller")
    p.add_argument("--kubectl", nargs="+", default=["kubectl"], help="Command prefix, for example --kubectl k3s kubectl")
    p.add_argument("--memory-limit", help="Controller container memory limit to apply with the image, for example 1Gi")
    p.add_argument("--retention-mode", choices=["dry-run", "enabled"])
    p.add_argument("--remote-mcp-mode", choices=["enable", "disable"], help="explicit MCP opt-in; enable only after staging the exact proxy location; omit to preserve current args")
    args = p.parse_args()
    if args.recover_crashloop and not custody_fds:
        p.error("crash recovery requires the guarded host deployment path")
    if args.memory_limit is not None and not re.fullmatch(r"[1-9][0-9]{0,5}(Mi|Gi)", args.memory_limit):
        p.error("memory limit must look like 512Mi or 1Gi")
    if not re.fullmatch(r"[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}", args.image) or not re.fullmatch(r"[0-9a-f]{40}", args.source_revision) or not 0 <= args.drain_seconds <= 3600:
        p.error("use an immutable image digest and a drain window from 0 through 3600 seconds")
    origin = urllib.parse.urlsplit(args.url)
    if origin.username or origin.password or origin.query or origin.fragment or origin.path not in ("", "/") or not origin.hostname or origin.scheme != "https" and not (origin.scheme == "http" and origin.hostname in ("127.0.0.1", "::1", "localhost")):
        p.error("url must be an HTTPS origin or loopback HTTP origin")
    if args.remote_mcp_mode == "enable":
        remote_mcp_args([], "enable", args.url)  # Refuse HTTP before credentials or external actions.
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
        result = subprocess.run(k + arguments, capture_output=True, text=True, timeout=timeout, **artifact_lease_options())
        if result.returncode:
            raise RuntimeError("Kubernetes operation failed; inspect deployment status before retrying")
        return result.stdout

    deployment = json.loads(command(["get", "deployment", args.deployment, "-o", "json"]))
    if deployment["spec"].get("replicas") != 1 or deployment["spec"].get("strategy", {}).get("type") != "Recreate":
        p.error("deployment must have one replica and Recreate strategy")
    containers = deployment["spec"]["template"]["spec"]["containers"]
    if sum(c["name"] == args.container for c in containers) != 1:
        p.error("selected controller container is unavailable")
    selected = next(c for c in containers if c["name"] == args.container)
    desired_args = remote_mcp_args(selected.get("args", []), args.remote_mcp_mode, args.url)
    desired_args = retention_args(desired_args, args.retention_mode)
    if not args.recover_crashloop:
        if api("GET", "/api/v1/instance").get("instance_id") != args.instance_id:
            p.error("unexpected service instance")
        api("GET", "/api/v1/operator/deployment-drain")  # Refuse unsupported old servers before changing anything.
    # Pull and start the image on the serving node before touching admission.
    # This pod has no live data, credentials, provider mounts or service token.
    labels = deployment["spec"]["selector"]["matchLabels"]
    selector = ",".join(key + "=" + value for key, value in labels.items())
    def selected_pods():
        return json.loads(command(["get", "pods", "-l", selector, "-o", "json"]))["items"]

    pods = selected_pods()
    if args.recover_crashloop:
        require_crashloop(api, deployment, pods, args.container)
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
        created = subprocess.run(k + ["create", "-f", "-"], input=json.dumps(spec), text=True, capture_output=True, timeout=30, **artifact_lease_options())
        if created.returncode:
            raise RuntimeError("image preflight could not start; live admission is unchanged")
        command(["wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/" + name, "--timeout=95s"], timeout=100)
    finally:
        command(["delete", "pod", name, "--ignore-not-found", "--timeout=30s"], timeout=35)
    began, replacing = False, False
    try:
        if args.recover_crashloop:
            require_crashloop(api, deployment, selected_pods(), args.container)
            forced = False
        else:
            began = True  # A lost POST response may still have started the drain.
            api("POST", "/api/v1/operator/deployment-drain")
            forced = wait_for_drain(lambda: api("GET", "/api/v1/operator/deployment-drain"), args.drain_seconds)
        # Prevent changing a deployment that another operator changed while we waited.
        deployment["spec"]["template"]["spec"]["terminationGracePeriodSeconds"] = 90
        for c in containers:
            if c["name"] == args.container:
                c["image"] = args.image
                c["imagePullPolicy"] = "IfNotPresent"
                if args.memory_limit:
                    c.setdefault("resources", {}).setdefault("limits", {})["memory"] = args.memory_limit
                if args.retention_mode is not None or args.remote_mcp_mode is not None:
                    c["args"] = desired_args
        annotations = deployment["spec"]["template"].setdefault("metadata", {}).setdefault("annotations", {})
        annotations["firecontroller.dev/source-commit"] = args.source_revision
        annotations["firecontroller.dev/image-reference"] = args.image
        annotations["firecontroller.dev/rollout-id"] = uuid.uuid4().hex
        annotations.pop("firecontroller.dev/binary-sha256", None)  # Old locally built binary provenance cannot describe this image.
        replacing = True  # A replace with a lost outcome must not resume old admission.
        # An in-flight replace must retain custody if this driver exits unexpectedly.
        result = subprocess.run(k + ["replace", "-f", "-"], input=json.dumps(deployment), text=True,
                                capture_output=True, timeout=30, **artifact_lease_options(custody_fds))
        if result.returncode:
            raise RuntimeError("replacement refused; inspect this deployment before cancelling drain or retrying")
        command(["rollout", "status", "deployment/" + args.deployment, "--timeout=120s"], timeout=130)
        current = json.loads(command(["get", "deployment", args.deployment, "-o", "json"]))
        installed = next(c for c in current["spec"]["template"]["spec"]["containers"] if c["name"] == args.container)
        if installed["image"] != args.image:
            raise RuntimeError("selected image changed during rollout")
        if (args.retention_mode is not None or args.remote_mcp_mode is not None) and installed.get("args", []) != desired_args:
            raise RuntimeError("selected Controller arguments changed during rollout")
        wait_for_recovery(lambda: (api("GET", "/api/v1/instance"), api("GET", "/api/v1/operator/deployment-drain")), args.instance_id)
        receipt = {"state": "deployed", "image": args.image, "drain_deadline_reached": forced}
        if args.recover_crashloop:
            receipt["recovered_crashloop"] = True
        print(json.dumps(receipt))
    finally:
        if began and not replacing:
            api("DELETE", "/api/v1/operator/deployment-drain")


if __name__ == "__main__":
    main()
