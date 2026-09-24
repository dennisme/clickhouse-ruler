# Checks

Every check this ruler can report, what it ships as, and where it is explained.

Generated from the check table in `internal/lint/checks.go`. Edit the table, then run `just generate`.

| Check | Severity | What it reports |
| --- | --- | --- |
| [`annotations/required`](rule.md#annotations-required) | `warning` by default | an annotation this repository requires on every alert is missing |
| [`annotations/runbook`](rule.md#annotations-runbook) | `warning` by default | a runbook_url that is not an absolute http or https URL |
| [`annotations/template`](rule.md#annotations-template) | `warning` by default | an annotation that is not a parseable template |
| [`labels/required`](rule.md#labels-required) | `warning` by default | a label this repository requires on every alert is missing |
| [`policy/check-limit`](policy.md#policy-check-limit) | fixed, always `error` | a ceiling that is not written as name:number, so it would never apply |
| [`policy/fixed-check`](policy.md#policy-fixed-check) | fixed, always `error` | a policy file configuring a correctness check, which cannot be softened |
| [`policy/severity`](policy.md#policy-severity) | fixed, always `error` | a severity that is not off, warn or error |
| [`policy/unknown-check`](policy.md#policy-unknown-check) | fixed, always `error` | a policy file configuring a check that does not exist |
| [`rule/columns`](rule.md#rule-columns) | fixed, always `error` | a query naming a column or table that does not exist, or returning no value column |
| [`rule/complexity`](rule.md#rule-complexity) | `warning` by default | a query with more joins or subqueries than the configured ceiling |
| [`rule/expr`](rule.md#rule-expr) | fixed, always `error` | an empty query, or one missing the time bounds the ruler binds |
| [`rule/for`](rule.md#rule-for) | `warning` by default | a `for` shorter than the group interval, so the alert fires on its first evaluation |
| [`rule/foreign-table`](rule.md#rule-foreign-table) | `warning` by default | a query reading a table outside its source's own database |
| [`rule/group-name`](rule.md#rule-group-name) | fixed, always `error` | a group with no name, or a name repeated within one file |
| [`rule/inspect`](rule.md#rule-inspect) | fixed, always `error` | the ruler could not reach the cluster to read the rule's SQL |
| [`rule/name`](rule.md#rule-name) | fixed, always `error` | an alert with no name, or a duplicate within its group, which has no identity |
| [`rule/nondeterministic`](rule.md#rule-nondeterministic) | `warning` by default | a query calling a function that breaks window alignment, such as now() |
| [`rule/protected-label`](rule.md#rule-protected-label) | fixed, always `error` | a rule setting a label the ruler owns, which breaks routing |
| [`rule/select-star`](rule.md#rule-select-star) | `warning` by default | a query selecting *, so a schema change rewrites every alert's identity |
| [`rule/settings`](rule.md#rule-settings) | fixed, always `error` | a query setting its own SETTINGS, overriding the limits the ruler sends |
| [`rule/source-match`](rule.md#rule-source-match) | `warning` by default | a rule whose selector matches no source, so this ruler will never evaluate it |
| [`rule/syntax`](rule.md#rule-syntax) | fixed, always `error` | SQL ClickHouse cannot parse, or a second statement nobody reviewed |
| [`rule/table-access`](rule.md#rule-table-access) | `warning` by default | a source's user cannot read what the rule asks for, so nothing could be checked |
| [`rule/table-function`](rule.md#rule-table-function) | `error` by default | a query reading through a table function the allowlist does not permit |
| [`rule/window`](rule.md#rule-window) | `warning` by default | a `window` shorter than the group interval, leaving data no evaluation reads |
| [`ruleset/directory`](policy.md#ruleset-directory) | fixed, always `error` | the rules directory could not be read |
| [`source/address`](source.md#source-address) | fixed, always `error` | a source with no address to connect to |
| [`source/database`](source.md#source-database) | fixed, always `error` | a source naming no database |
| [`source/evaluation-delay`](source.md#source-evaluation-delay) | fixed, always `error` | a negative evaluation delay |
| [`source/exemption`](source.md#source-exemption) | fixed, always `error` | an exemption that is malformed, names a check nobody can exempt, or has expired |
| [`source/max-execution-time`](source.md#source-max-execution-time) | fixed, always `error` | an execution time cap that is not positive |
| [`source/max-memory-usage`](source.md#source-max-memory-usage) | fixed, always `error` | a memory cap that is not positive |
| [`source/max-rows`](source.md#source-max-rows) | fixed, always `error` | a row cap that is not a positive number |
| [`source/name`](source.md#source-name) | fixed, always `error` | a source with no name, or a name used twice |
| [`source/password`](source.md#source-password) | fixed, always `error` | a secret that could not be read, or both secret sources set at once |
| [`source/privileges`](source.md#source-privileges) | `warning` by default | a source's ClickHouse user does not meet the contract the other checks rely on |
| [`source/table`](source.md#source-table) | fixed, always `error` | a source naming no table |
| [`source/timestamp-column`](source.md#source-timestamp-column) | fixed, always `error` | a source naming no timestamp column, so no window can be bound |
| [`source/username`](source.md#source-username) | fixed, always `error` | a source naming no ClickHouse user, which is the tenancy boundary |
| [`yaml/syntax`](policy.md#yaml-syntax) | fixed, always `error` | the file is not valid YAML, so nothing in it could be read |
| [`yaml/type`](policy.md#yaml-type) | fixed, always `error` | a field holding the wrong shape, such as a list where a mapping belongs |
| [`yaml/unknown-field`](policy.md#yaml-unknown-field) | fixed, always `error` | a field nobody recognises, which silently drops configuration |
