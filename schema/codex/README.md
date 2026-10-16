# Codex app-server schema

Each directory here holds the JSON Schema bundle for one Codex release,
generated with:

```sh
codex app-server generate-json-schema --experimental --out schema/codex/<version>
```

These files are output of [openai/codex](https://github.com/openai/codex),
which is licensed under the Apache License 2.0 (copy in `LICENSE-APACHE-2.0`).
They are redistributed unmodified, as reference data for the protocol contract
test in `internal/codex/contract_test.go`. Recorded protocol traffic in
`internal/codex/testdata/` is Codex output too, and the same notice applies.

The rest of Switchworks is licensed under GPL-3.0-or-later (see `/LICENSE`).
