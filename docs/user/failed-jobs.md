# Failed jobs

When a job fails, Labmill finds out why.

## Ask

Type "why did job 12 fail?". You can also use the job list in the desktop app: click **Logs** or **Status**.

## What Labmill looks at

Labmill collects facts only. It does not change anything on the cluster. The facts are:

- the state of the job and its history in Labmill;
- the record of the scheduler: state, exit code, reason, time used against the limit, memory used against the memory you asked for;
- the end of the job log (the last 60 lines), with secrets hidden;
- the start of the job script.

Then the assistant names the likely cause in plain words. It uses the failure patterns in Labmill's skills. For example: out of memory, out of time, a missing module, a file that was not found.

## Fix and resubmit

Labmill offers to fix the problem and submit the job again. You see the new script and approve it, as for any job.

## Memory on your computer

Labmill remembers the cause in its job database on your computer only. The next time you draft a job from the same folder on the same cluster, it tells you that an earlier run failed and why. The cause is not sent anywhere except in the chat.

## Preempted jobs

If a job runs on a preemptible partition, Labmill can submit it again after a preemption, at most twice, then once on a regular partition. You see this plan before you approve the job.

## When the job never starts

Before you submit, the job review says whether Slurm accepts the script and when it expects the job to start. If Slurm refuses the script, the box shows Slurm's error. You can fix the script first.
