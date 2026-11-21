# brood panel

This directory holds the source of the brood web panel: a React 19 +
TypeScript + Vite application that is built into a single `index.html`
and embedded in the `brood` server binary. It is not deployed on its own.

What the panel does, how its pages work and how to build it into the
server is described in [docs/WEB.md](../docs/WEB.md). Conventions for
changing the code are in [AGENTS.md](AGENTS.md).

```
bun install --frozen-lockfile
bun run type-check
bun run lint
bun run build        # writes dist/index.html
```

## Origin

The panel started as
[CLI Proxy API Management Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center),
the management UI of [CLI Proxy API](https://github.com/router-for-me/CLIProxyAPI),
by Router-For.ME, used under the MIT License (see [LICENSE](LICENSE)).
Its pages for managing the model gateway (providers, auth files, OAuth,
quota, configuration, logs, plugins) are kept; brood added its own
sections (projects, swarms, agents, pools, users, profile) and replaced
the management-key sign-in with the brood server session.
