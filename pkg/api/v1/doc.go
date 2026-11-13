// Package v1 holds the public wire contract of docker-offload-lan.
//
// Everything in this package is a PUBLIC CONTRACT shared by the client
// (`dol mcp`, `dol` CLI) and the node agent (`dol node serve`): HTTP request
// and response bodies of the node API, error codes, Docker label names and
// the structured output of MCP tools.
//
// Do not change field names, JSON tags, error codes or label names without an
// ADR in docs/adr/ (see docs/IMPLEMENTATION_BRIEF.md §0 and §14). Adding new
// optional fields is backwards compatible; renaming or removing is not.
package v1

// APIVersion is the node API version served under the /v1 prefix.
const APIVersion = "v1"

// PathPrefix is the URL prefix for every node API route.
const PathPrefix = "/v1"
