# Compute-node networking: HTTP proxies and reaching GitHub

On many clusters, **compute nodes reach the outside world only through an HTTP
proxy**, and the login nodes route more directly. Since real work runs on
compute nodes, this matters constantly. The proxy address, the internal domain
that causes trouble in `no_proxy`, and whether port 22 is open are in your site
skill (`site-<institution domain>`).

## See what the node gives you

```bash
env | grep -i _proxy
```

Typical output:

```
http_proxy=http://<proxy-host>:<port>
https_proxy=http://<proxy-host>:<port>
no_proxy=localhost,127.0.0.1,.<internal-domain>,<node-ip>
```

Tools that honor `HTTP(S)_PROXY`/`NO_PROXY` (curl, wget, git, pip, Python
`requests`, most Go binaries) use these automatically. Some read only the
lower-case or only the upper-case names, so set both when you change them.

## The `no_proxy` trap

`no_proxy` lists hosts to reach **directly**, bypassing the proxy. If it
contains the site's internal domain (e.g. `.<internal-domain>`), every host
under that domain is contacted directly. From a compute node those internal
addresses are often not routable, so the connection **hangs until it times
out** instead of using the working proxy. This is a common cause of "the CLI
hangs on the cluster but works on the login node".

**Fix: strip just that one entry** and keep the rest (the last entry is often a
node-specific IP, so don't hardcode the whole list):

```bash
dom='.<internal-domain>'        # the domain your site skill names
export no_proxy="$(printf '%s' "$no_proxy" | tr ',' '\n' | grep -vxF -- "$dom" | paste -sd, -)"
export NO_PROXY="$no_proxy"
```

To do it automatically in every **interactive** Slurm job, add to `~/.bashrc`
(not `~/.bash_profile`; `srun --pty bash` reads only `.bashrc`):

```bash
[ -n "$SLURM_JOB_ID" ] && { export no_proxy="${no_proxy//.<internal-domain>/}"; export NO_PROXY="$no_proxy"; }
```

`SLURM_JOB_ID` is set in any job, but a **batch script reads no `.bashrc`**
(`#!/bin/bash`), so for `sbatch` put the same line inside the script after the
`#SBATCH` lines, or start it with `#!/bin/bash -l` and have `~/.bash_profile`
source `~/.bashrc`. Quote variables in `[ -n "$VAR" ]`: unquoted, an empty variable collapses to
`[ -n ]`, which is always true.

To make one CLI work in every interactive shell, also outside a Slurm job, add
an alias to `~/.bashrc` that clears the entry for that command only (aliases
don't apply in batch scripts; use the export above there):

```bash
alias mycli='no_proxy="${no_proxy//.<internal-domain>/}" NO_PROXY="${no_proxy//.<internal-domain>/}" command mycli'
```

For a one-off `curl`, `curl --noproxy '' <url>` also works, but that flag is
curl-only; other tools need the environment fixed.

Quick check:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' https://api.github.com   # 200 = proxy path works
```

## Reaching GitHub

### HTTPS (simplest)

`https://github.com/...` remotes go through `https_proxy`, so clone and fetch
work as soon as the proxy variables are right. Pushing over HTTPS needs a
credential: a personal access token, or `gh auth login` (the `gh` CLI also
honors the proxy).

### SSH when outbound port 22 is blocked

Many sites firewall outbound SSH on port 22 from compute nodes, so a
`git@github.com:` remote hangs. If the proxy allows `CONNECT` to port 443,
tunnel to GitHub's SSH endpoint **`ssh.github.com:443`**.

1. Save a small CONNECT connector as `~/.ssh/proxy_connect.py` and
   `chmod +x` it:

   ```python
   #!/usr/bin/env python3
   """HTTP CONNECT connector for use as an SSH ProxyCommand.
   Usage: proxy_connect.py <host> <port>
   Proxy from PROXY_HOSTPORT (host:port), else from https_proxy/http_proxy."""
   import os, sys, socket, threading
   from urllib.parse import urlparse

   def proxy_addr():
       hp = os.environ.get("PROXY_HOSTPORT")
       if hp:
           h, p = hp.rsplit(":", 1)
           return h, int(p)
       for name in ("https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY"):
           if os.environ.get(name):
               u = urlparse(os.environ[name])
               return u.hostname, u.port or 3128
       sys.exit("no proxy configured (set PROXY_HOSTPORT)")

   def main():
       host, port = sys.argv[1], int(sys.argv[2])
       s = socket.create_connection(proxy_addr(), timeout=20)
       s.sendall(f"CONNECT {host}:{port} HTTP/1.1\r\nHost: {host}:{port}\r\n\r\n".encode())
       buf = b""
       while b"\r\n\r\n" not in buf:
           c = s.recv(1)
           if not c:
               sys.exit("proxy closed during CONNECT")
           buf += c
       status = buf.split(b"\r\n", 1)[0].decode(errors="replace")
       if " 200 " not in status:
           sys.exit(f"proxy CONNECT failed: {status}")
       out_fd, in_fd = sys.stdout.fileno(), sys.stdin.fileno()
       def s2o():
           try:
               while True:
                   d = s.recv(65536)
                   if not d: break
                   os.write(out_fd, d)
           except Exception: pass
       def i2s():
           try:
               while True:
                   d = os.read(in_fd, 65536)   # NOT sys.stdin.read(n): that blocks for a full buffer
                   if not d: break
                   s.sendall(d)
           except Exception: pass
       t = threading.Thread(target=s2o, daemon=True); t.start()
       i2s()
       try: s.shutdown(socket.SHUT_WR)
       except Exception: pass
       t.join(timeout=5)

   if __name__ == "__main__":
       main()
   ```

   The relay uses `os.read`, not `sys.stdin.buffer.read(n)`: the latter blocks
   until it has a full buffer, which stalls the SSH handshake and looks like a
   hang.

2. Add to `~/.ssh/config` on the cluster (use absolute paths; `~` is not
   expanded inside `ProxyCommand`):

   ```
   Host github.com
       HostName ssh.github.com
       Port 443
       User git
       ProxyCommand /path/to/python3 /home/<you>/.ssh/proxy_connect.py %h %p
   ```

   ProxyCommand runs without your job's environment fixes, so if the proxy
   variables aren't set where ssh runs, put `PROXY_HOSTPORT=<proxy-host>:<port>`
   in front of the python path.

3. Register `~/.ssh/id_ed25519.pub` at <https://github.com/settings/keys> as an
   **Authentication key**. A key added only as a *Signing* key gives the
   misleading `Server accepts key` followed by `Permission denied (publickey)`.

Verify: `ssh -T git@github.com` should answer `Hi <user>!`; then `git clone`,
`fetch` and `push` for `git@github.com:` remotes work over port 443.

If the cluster's network allows outbound port 22, none of this is needed. Test
with `ssh -T -o ConnectTimeout=10 git@github.com`.
