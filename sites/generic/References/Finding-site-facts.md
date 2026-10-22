# Finding site-specific facts without a site skill

Each row names a fact the generic skills defer to a site skill, who knows it,
and how to discover it. Prefer discovery over asking, and asking over
guessing.

| Fact | Ask the user / their docs | Discover it |
|---|---|---|
| Login (submit) host | "What do you type after `ssh`?" Research-computing docs, "connecting" page | `hostname -f` once logged in |
| Jump host / VPN | Docs: off-campus access | A login that only works on campus or VPN suggests a VPN or a gateway (`ProxyJump`) is needed |
| Transfer host | Docs: "data transfer", "DTN", "file transfer" | `sftp <login-host>` failing with "subsystem request failed" means the login host has SFTP turned off; look for a dedicated transfer host |
| Account name and 2FA | The institution's IT pages (single sign-on name, Duo/TOTP/push) | The login prompt itself shows the 2FA menu |
| Slurm partitions | Docs: "queues" / "partitions" | `sinfo -s`, `sinfo -o "%P %G %l %D"` (GRES = GPUs, time limit, nodes) |
| GPU types | Docs: "GPU nodes" | `sinfo -o "%N %G"` |
| Slurm account / QOS | The user's PI or group | `sacctmgr show assoc user=$USER format=account,partition,qos` |
| Storage folders and quotas | Docs: "storage", "scratch", "quotas" | `echo $HOME`, `df -h $HOME`, `ls /` for scratch or project mounts; quota commands vary by file system |
| HTTP proxy on compute nodes | Docs: "internet access from compute nodes" | `env | grep -i proxy` inside a job |
| Node driver / CUDA version | Docs: "GPU software" | Inside a GPU job: `nvidia-smi --query-gpu=name,driver_version --format=csv` |
| Container images | Docs: "containers", "Apptainer" | `module avail apptainer` or `command -v apptainer`; an image pull that fails with "unauthorized" means a registry credential is needed |
| Software modules | Docs: "software", "modules" | `module avail`, `module spider <name>` |
| Software outside modules | Docs: "software", "installed programs"; ask support | Where the site installs software apart from modules: the shared folders, how to pin a version, how to list it (`ls <folder>`, `command -v <name>`) |

When the user confirms a fact, keep using it for the rest of the session and
say which value you are using.
