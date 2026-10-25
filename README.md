# FireController

[![License: Apache-2.0](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)
[![Status: Active development](https://img.shields.io/badge/Status-Active_development-yellow)](docs/delivery.md)

**A clear view of your AI agents, their jobs, and your review and merge trains.**

FireController is a self-hosted coordination service for AI-assisted software development on GitHub projects. It gives agents shared jobs, durable messages and review history, with a live project website for the people overseeing their work.

Use it when several agents are working across chats and worktrees, or when one agent needs to pick up a project across many sessions. Assignments, working context and outstanding findings stay with the project, ready for the next agent or session to read.

## Why use FireController?

- **Keep track of who is doing what.** The Delivery page brings worker lanes, assigned jobs, unassigned work and the review queue together. See the current work and merge-train head without opening every agent's chat.
- **Make handoffs useful.** Give a job a brief, current progress, next action and an optional checklist. Agents read that working context directly; history is retained automatically. Attached notes keep distinct reminders visible until they are resolved.
- **Let agents coordinate.** Workers exchange durable messages through their own CLI or MCP connections. Messages survive reconnects, and reading a message is separate from marking it handled. Quiet waits let a capable host collect incoming messages while other work continues.
- **Manage review and merge trains.** Follow ordered or independent PRs, CodeRabbit Hosted and CLI rounds, and recorded subagent reviews. One chronological history shows findings, accepted Major/Minor/Trivial counts, decisions and fix evidence. Configure round limits and early stopping, route findings to related work, and keep completed reviews as the code moves forward. You retain merge authority.
- **Share the project's direction.** Publish an overview, roadmap and selected progress at the same project URL people use to sign in. Public content is deliberately selected; private briefs, messages, notes and finding bodies stay private.

After project registration, everyday jobs and messages work independently of GitHub availability. The service handles configured GitHub observation and review execution in the background, so an ordinary inbox or job read does not repeatedly fetch PRs or wait for a provider.

## A typical workflow

1. Your existing lead agent assigns jobs to workers. Give the lead and each builder its own bound connection and a short handover pointing to its job and usage instructions.
2. A builder reads its assigned job when resuming, updates current progress for the next handoff, and messages the lead when another agent needs to act. Earlier progress remains in history.
3. PRs enter the review queue. Agents inspect returned findings, record their decisions and attach fix evidence. Finalizing a native review automatically queues its GitHub summary.
4. You follow progress in the browser, inspect the mixed review timeline and decide when to merge. Merged PRs remain in queue history.

A solo agent can keep a feature's brief and next action on one job; a later chat resumes by connecting as the same worker and reading that job.

Start with jobs and messaging, and add review execution when you need it. FireController works with your chosen agent host and models through a CLI, a stdio MCP adapter and the service API.

Your agent host starts and resumes the chats; FireController provides the shared working context they read and update. See the [handoff workflow](docs/agent-workflow.md#messages-and-handoffs). Review execution has a separate [setup guide](docs/review-execution.md#operator-capabilities-and-project-configuration): configure the service's provider connection, then the project's queue and review channels.

## Built for agents

Each private profile binds an instance, project and worker. A generated launcher keeps that identity fixed, including when agents share a checkout. Everyday calls stay short:

```sh
fc-builder doctor
fc-builder jobs assigned
fc-builder inbox list
fc-builder reviews queue --active
```

`fc-builder` is an example bound launcher created during setup. MCP connections carry the same binding and expose native tools for jobs, messages and reviews.

Routine results are structured and bounded. Full briefs, findings and archives are explicit reads. Fast operations return directly; variable-duration work allows a bounded wait and returns a durable operation handle when needed. Actual background delivery and idle-agent wakeups depend on the agent host.

The bundled [everyday-use skill](skills/firecontroller-use/SKILL.md) teaches the supported workflow. The [agent client guide](docs/agent-client.md) and [MCP guide](docs/mcp.md) cover the integration details.

## Get started

Each managed project needs a GitHub repository. First-time registration verifies that repository through GitHub; private repositories also need a configured service credential. See [project registration and access](docs/local-api.md#project-and-worker-setup).

### Ask your agent to set it up

Give your agent a FireController checkout and point it to the [onboarding skill](skills/firecontroller-onboard/SKILL.md). For example:

> My project is at `/path/to/my-project`, and the FireController checkout is at `/path/to/FireController`. Read FireController's onboarding skill and connect my project to an existing or new local instance. Set up private worker connections, verify a job and message round trip, and help me choose which history to bring in and what to share publicly.

The skill guides connection to an existing instance or setup of a local one, project registration, private agent profiles and verification. It preserves existing project instructions. Historical imports have explicit scope and preview steps; you can begin with new work. See the [onboarding guide](docs/onboarding.md).

A successful setup leaves each agent with a verified connection and gives you a project URL and private browser access. The agent should explain where the credentials are stored and demonstrate the first job and message round trip.

### Run a local instance from source

Use Go 1.27, Node.js 22.22 or newer, and Make on Linux or WSL:

```sh
git clone https://github.com/benhook1013/FireController.git
cd FireController
npm --prefix webapp ci --ignore-scripts --no-audit --no-fund
make build
./bin/fcd --data-dir "$HOME/.local/share/firecontroller" --listen 127.0.0.1:8080
```

Open [http://127.0.0.1:8080](http://127.0.0.1:8080). This starts an empty local instance; use the onboarding skill or [project and worker setup](docs/local-api.md#project-and-worker-setup) to create your first project and agent connections. First startup writes the administrator credential to `bootstrap-admin.token` in the private data directory. Keep that credential private.

The build produces `fcd` (the service with its embedded website), `fc` (the CLI) and `fcmcp` (the agent adapter). The browser needs no separate web server. [Development instructions](docs/development.md) cover other platforms, toolchain setup and validation.

## Host it your way

One installation can serve several projects, each with its own workers, jobs, messages, review configuration and appearance. Each project binds one GitHub repository. Run it on a single computer or host it on a network server with HTTPS; local and remote clients use the same API.

The service and clients are written in Go, the embedded browser uses React and TypeScript, and durable state lives in SQLite on local storage. The current deployment model is one active service replica. Multiple active replicas and distributed review executors are future work.

Managed local backups provide verified bundles, scheduling and retention. Optional offsite backups use rclone, including cloud storage and encrypted destinations, with separate credentials and retention. The website can display backup health. See [hosting and HTTPS](docs/hosting.md), the [container guide](docs/container-runtime.md), [backups](docs/backups.md) and [offsite recovery](docs/offsite-backups.md).

## Project status and contributing

FireController is in active development before its first stable release. It is already used for real project coordination and reviews; interfaces and setup are still evolving. The [delivery plan](docs/delivery.md) and individual feature guides record implemented behavior, validation and remaining limits.

Feedback from actual agent workflows is especially useful: confusing output, repeated context, unnecessary commands, slow coordination, difficult handoffs and recovery problems. Include the workflow, what you expected and what happened, with private data removed.

For a documentation-only change, install the root dependencies and run the Markdown and link checks:

```sh
npm ci --ignore-scripts --no-audit --no-fund
npm run docs:check
```

For application changes, follow the [development guide](docs/development.md). AI contributors should read [AGENTS.md](AGENTS.md). The [documentation index](docs/README.md) links the product design, API contracts and feature guides.

## License

Copyright 2026 Benjamin James Hook. New FireController material is licensed under [Apache-2.0](LICENSE). Referenced historical source retains its original license. The bundled Inter header font uses the SIL Open Font License; its [copyright and license](webapp/public/fonts/Inter-OFL.txt) accompany the packaged website.
