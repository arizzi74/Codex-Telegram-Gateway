# JSON configuration

Context: the specification's examples use YAML but do not require that format.

Decision: use strict JSON with unknown-field rejection and human-readable duration
strings. Resolve relative paths against the configuration directory. Secrets
remain separate environment values or owner-readable files.

Consequences: no YAML parser is needed. Samples live in examples/. Worker
configuration and token files must have owner-only permissions.
