# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `agent-platform.giantswarm.io/tool-group: agent-platform` on the muster `MCPServer` CR, next to `muster.giantswarm.io/type`: model-manager is part of the Agent Platform's own management surface, the tier the portal's MCP servers page, the toolset presets and the docs group it under. `muster.mcpServer.labels` still adds to and overrides the CR's labels; the passthrough is merged instead of appended, so an override renders one key instead of a duplicate.


[Unreleased]: https://github.com/giantswarm/model-manager/tree/main
