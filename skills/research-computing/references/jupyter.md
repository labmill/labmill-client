# Jupyter notebooks on a cluster

Never run Jupyter on a shared login host: it holds memory and CPU that other
people need, and long-running processes there get killed. Run it in a job on a
compute node and reach it from your computer through an SSH tunnel. If the site
offers a web portal that starts notebooks for you, that is easier (see your
site skill).

## A kernel for your environment

Install Jupyter into the environment your code uses (see environments.md), then
register that environment as a kernel so notebooks start with its packages:

```bash
source /path/to/project/.venv/bin/activate
python -m pip install jupyterlab ipykernel
python -m ipykernel install --user --name myproject --display-name "Python (myproject)"
jupyter kernelspec list                       # confirm it is registered
```

(`--user` here belongs to `ipykernel install`, which writes the kernel
description into `~/.local/share/jupyter`; it's not the `pip --user` flag and
is correct inside a venv.) For conda: `conda activate …` then the same
`ipykernel install` line. Kernel specs are tiny; the environment itself can
live in project space.

## Notebook in a Slurm job, with an SSH tunnel

1. **Start a job that runs Jupyter** (batch, so it survives disconnects):

   ```bash
   cat > jupyter.sbatch <<'EOF'
   #!/bin/bash
   #SBATCH --job-name=jupyter
   #SBATCH --time=04:00:00
   #SBATCH --cpus-per-task=4
   #SBATCH --mem=16G
   #SBATCH --output=jupyter_%j.log
   source /path/to/project/.venv/bin/activate
   PORT=$(python -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1])')
   echo "NODE=$(hostname) PORT=$PORT"
   jupyter lab --no-browser --ip="$(hostname -s)" --port=$PORT
   EOF
   sbatch jupyter.sbatch
   ```

   Add `--partition`, `--account` or a GPU request as your site needs (see the
   slurm-user skill and your site skill).

2. **Read the node, port and token** once the job runs
   (`squeue --me`, then `cat jupyter_<jobid>.log`). The log contains
   `NODE=<node> PORT=<port>` and a URL with `?token=<token>`.

3. **Tunnel from your computer** (a new terminal on your own machine, not on
   the cluster):

   ```bash
   ssh -N -L 8888:<node>:<port> <login-host-alias>
   ```

   `<login-host-alias>` is the name you use for `ssh` (see ssh.md). Leave it
   running. If the login host asks for 2FA each time, use ControlMaster
   (ssh.md) so the tunnel reuses an open login.

4. **Open** `http://127.0.0.1:8888/lab?token=<token>` in your browser. The
   token is a password to your account: don't paste it into chat or tickets.

5. **Stop** it when done: `scancel <jobid>`. An idle notebook holds a node.

Notes:

- Bind to the node's own name (`--ip="$(hostname -s)"`), not `127.0.0.1`: the
  tunnel enters through the login host, which connects to the node over the
  network, and a loopback-only server refuses that. The token still protects
  the notebook; avoid `--ip=0.0.0.0`, which listens on every interface.
- Where the site lets you SSH straight to a node that runs your job, you can
  keep `--ip=127.0.0.1` and tunnel to the node itself:
  `ssh -N -L 8888:127.0.0.1:<port> -J <login-host-alias> <node>`.
- `ssh -J`/`ProxyJump` setups work the same: the alias carries the jump.
- If the compute node isn't reachable from the login host by name, use the
  node's short hostname exactly as `squeue` shows it.
- Port already in use: pick a different local port (`-L 8899:…`).

## Interactive session instead of batch

For a short session: `srun --pty --time=02:00:00 --cpus-per-task=4 bash`, run
`hostname -s` and `jupyter lab --no-browser --ip="$(hostname -s)" --port=<free port>`
inside, then tunnel as above. It ends when
your terminal drops, so use batch or `tmux` (ssh.md) for anything long.

## From notebook to batch script

Notebooks are for exploring; long or repeated runs belong in a script:

```bash
python -m pip install nbconvert
jupyter nbconvert --to script analysis.ipynb      # -> analysis.py
```

Review the result: remove `get_ipython()` magics and inline plotting calls, put
settings in command-line arguments, and write outputs to files. Then run it
under `sbatch` like any program. To run a notebook headless and keep its
output cells: `jupyter nbconvert --to notebook --execute analysis.ipynb`, or
`python -m pip install papermill` and `papermill in.ipynb out.ipynb -p alpha 0.1`
for parameterised runs (arrays of such jobs are a good fit for Slurm job
arrays).
