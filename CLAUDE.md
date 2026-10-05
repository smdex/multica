@AGENTS.md

<!-- waggle:stub:begin (managed by `waggle init`; edits inside are overwritten) -->
## Artifact handoffs (waggle)
When passing work products between agents or subagents, do not paste file
contents. Call waggle's `mint` with the artifact's path and hand over the
`handoff` line from the result. Consumers call `resolve` with the token.
For SOURCE CODE, mint with `snapshot` (structure is extracted: consumers
get a symbol outline and `read --symbol NAME`), and declare what a
consumer must reach — `--require symbol:NAME` — so `coverage` can prove
the review; judge returned work with `record --stage accepted|rejected`.
When minting a binary artifact (PDF, image, audio), extract its text with
your own abilities first and pass it via `content`.
If unsure what to do with a token, call `map`.
<!-- waggle:stub:end -->
