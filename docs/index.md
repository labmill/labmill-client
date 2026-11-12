# Labmill documentation

Labmill runs your research jobs on HPC clusters. It has two parts:

- **[Labmill client](user/index.md)**: the program on your computer. You say what you want to run in a chat, and the client finds your data, moves it to a Slurm cluster, runs the job, and brings the results back. It uses your own cluster account.
- **[Labmill service](service/index.md)**: the web app at [app.labmill.org](https://app.labmill.org). It knows the HPC systems and their facts, and keeps your job records for you and your research computing team.

## Download the client

These links are for the newest test build, v0.1.0-test.2. Labmill is under active development. Read [Install](user/install.md) first.

| Your computer | Desktop app | Terminal app |
|---|---|---|
| Windows (64 bit, Intel or AMD) | [`labmill-gui-windows-amd64.exe`](https://github.com/labmill/labmill-client/releases/download/v0.1.0-test.2/labmill-gui-windows-amd64.exe) | [`labmill-windows-amd64.exe`](https://github.com/labmill/labmill-client/releases/download/v0.1.0-test.2/labmill-windows-amd64.exe) |
| macOS (Apple silicon) | [`labmill-gui-darwin-arm64`](https://github.com/labmill/labmill-client/releases/download/v0.1.0-test.2/labmill-gui-darwin-arm64) | [`labmill-darwin-arm64`](https://github.com/labmill/labmill-client/releases/download/v0.1.0-test.2/labmill-darwin-arm64) |
| macOS (Intel) | [`labmill-gui-darwin-amd64`](https://github.com/labmill/labmill-client/releases/download/v0.1.0-test.2/labmill-gui-darwin-amd64) | [`labmill-darwin-amd64`](https://github.com/labmill/labmill-client/releases/download/v0.1.0-test.2/labmill-darwin-amd64) |
| Linux (64 bit, Intel or AMD) | [`labmill-gui-linux-amd64`](https://github.com/labmill/labmill-client/releases/download/v0.1.0-test.2/labmill-gui-linux-amd64) | [`labmill-linux-amd64`](https://github.com/labmill/labmill-client/releases/download/v0.1.0-test.2/labmill-linux-amd64) |

Other builds (Windows on Arm, Linux on Arm) and the checksum file are on the [Install](user/install.md) page.

## Next steps

- [Your first job](user/first-job.md)
- [User guide](user/index.md)
- [Labmill service](service/index.md): accounts, systems and facts, the API
- [Developer pages](dev/index.md), if you want to build or change the client
