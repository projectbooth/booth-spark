# Runs as a booth-spark application's own code, in its driver: what user code can and cannot do
# from inside a run (isolation.sh, egress.sh). It uses only what any user code has there: the
# driver's ServiceAccount token, the network, and Python's standard library. Every refusal it
# checks has a control that must succeed, so a probe that can't see (no token, no network) fails
# instead of passing.
#
#   main.py <mode> <json args>      mode: isolation | egress
# Prints one line: PROBE <json results>, then exits 0.
import json
import socket
import ssl
import sys
import urllib.error
import urllib.request

SA = "/var/run/secrets/kubernetes.io/serviceaccount"
mode, args = sys.argv[1], json.loads(sys.argv[2])
own = open(SA + "/namespace").read().strip()
token = open(SA + "/token").read().strip()
ctx = ssl.create_default_context(cafile=SA + "/ca.crt")
results = {}


def api(method, path, body=None):
    req = urllib.request.Request("https://kubernetes.default.svc" + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, context=ctx, timeout=10) as r:
            return r.status, r.read().decode()[:400]
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:400]
    except Exception as e:  # noqa: BLE001 - a probe reports every outcome
        return 0, type(e).__name__ + ": " + str(e)[:200]


def tcp(host, port, timeout=4):
    try:
        socket.create_connection((host, port), timeout=timeout).close()
        return "open"
    except Exception as e:  # noqa: BLE001
        return "closed:" + type(e).__name__


def pod(name, image, sa="executor", automount=False, workspace=None, node_selector=True, ns=None):
    spec = {
        "serviceAccountName": sa,
        "automountServiceAccountToken": automount,
        "restartPolicy": "Never",
        "securityContext": {"runAsNonRoot": True, "runAsUser": 185, "seccompProfile": {"type": "RuntimeDefault"}},
        "containers": [{
            "name": "c", "image": image, "command": ["sleep", "5"],
            "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
            "resources": {"limits": {"cpu": "100m", "memory": "64Mi"}},
        }],
    }
    if node_selector:
        spec["nodeSelector"] = args["nodeSelector"]
    body = {"apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": name, "labels": {"booth.projectbooth.io/workspace": workspace or args["workspace"]}},
            "spec": spec}
    code, text = api("POST", "/api/v1/namespaces/%s/pods" % (ns or own), body)
    if code == 201:
        api("DELETE", "/api/v1/namespaces/%s/pods/%s" % (ns or own, name))
    return code, text


if mode == "isolation":
    other = args["otherNamespace"]
    results["control: list pods in its own namespace"] = api("GET", "/api/v1/namespaces/%s/pods" % own)[0]
    results["list pods in run B's namespace"] = api("GET", "/api/v1/namespaces/%s/pods" % other)[0]
    results["read run B's Secrets"] = api("GET", "/api/v1/namespaces/%s/secrets" % other)[0]
    results["read run B's namespace"] = api("GET", "/api/v1/namespaces/%s" % other)[0]
    results["read Secrets in its own namespace"] = api("GET", "/api/v1/namespaces/%s/secrets" % own)[0]
    results["create a pod in run B's namespace"] = pod("probe-b", args["image"], ns=other)[0]
    results["control: its own driver port through its Service"] = tcp("driver.%s.svc" % own, 7078)
    results["run B's driver RPC port"] = tcp("driver.%s.svc" % other, 7078)
    results["run B's driver UI port"] = tcp("driver.%s.svc" % other, 4040)
    results["control: a compliant executor-like pod"] = pod("probe-ok", args["image"])[0]
    for name, kw in {
        "a pod with a non-allowlisted image": dict(image=args["otherImage"]),
        "a pod without the configured nodeSelector": dict(image=args["image"], node_selector=False),
        "a pod as the driver account": dict(image=args["image"], sa="driver", automount=True),
        "a pod as the default account": dict(image=args["image"], sa="default"),
        "an executor-account pod with a token mounted": dict(image=args["image"], automount=True),
        "a pod labelled with another workspace": dict(image=args["image"], workspace="other-team"),
    }.items():
        code, text = pod("probe-x", **kw)
        results[name] = "%d %s" % (code, "booth-spark:" in text and text[text.index("booth-spark:"):][:90] or text[:90])
elif mode == "egress":
    try:
        results["control: DNS"] = socket.gethostbyname("kubernetes.default.svc.cluster.local")
    except Exception as e:  # noqa: BLE001
        results["control: DNS"] = "fail:" + type(e).__name__
    results["control: the Kubernetes API"] = api("GET", "/version")[0]
    results["control: its own driver port"] = tcp("driver.%s.svc" % own, 7078)
    for name, (host, port) in args["targets"].items():
        results[name] = tcp(host, port)
print("PROBE " + json.dumps(results), flush=True)
