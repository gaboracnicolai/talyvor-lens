# Deletion requests — deleting everything Talyvor holds for a user or company

B21.3. A workspace can delete its stored answers itself, or ask Talyvor to delete everything. This is
the operator's procedure for the second: which stores hold a workspace's data, and how each is deleted.

## 1. See the request

A workspace owner files one with `POST /v1/workspaces/{ws}/deletion-requests` (the suite's Features page
does this). It records who asked and when; the owner sees its status with
`GET /v1/workspaces/{ws}/deletion-requests`.

On the server, inside the lens container:

```sh
docker compose exec lens lens deletion-requests        # open requests
docker compose exec lens lens deletion-requests all    # every request, done ones included
```

or with the admin key: `GET /v1/admin/deletion-requests` (`?all=true` for done ones).

## 2. Complete it in Lens

```sh
docker compose exec lens lens deletion-requests complete <id>
```

or `POST /v1/admin/deletion-requests/{id}/complete` with the admin key. This deletes, for that workspace:

| What | Where |
|---|---|
| its shared answers, variants included | `prompt_embeddings` where `is_poolable` and `contributor_workspace_id` = the workspace |
| its private answers | `prompt_embeddings` where `NOT is_poolable` and `workspace_id` = the workspace |
| its shared document conversions | Redis `lens:distill:*` keys whose `:owner` marker names the workspace |
| its private document conversions | Redis `lens:distill:*` keys whose `:ws` marker names the workspace |
| every cached copy of its answers | Redis `lens:exact:*` keys whose `:owner` marker names the workspace |

It then marks the request `done` with the time, writes a row to `stored_answer_deletions` (who, when, what
was deleted), and prints what it deleted. The owner's status read now shows `done`.

**Kept, and the completion says so:** billing and ledger records — `lxc_ledger`, `lens_token_ledger`,
`pool_royalty_mints` and the other tables `internal/tenantdata/manifest.go` marks `Retain` — because the law
requires them. Earnings already final are not clawed back. Answers already given to other users stay in
their conversations; nothing claims otherwise.

## 3. The rest of Lens

Stored answers are the most sensitive thing Lens holds, not the only thing. Every Lens table that holds a
workspace's data, and whether it is deleted or kept, is listed in `internal/tenantdata/manifest.go`, and a
test fails the build when a table is missing from it. To remove the workspace from Lens entirely, run one
`DELETE … WHERE <tenant column> = '<workspace>'` per table marked `Delete` — the tenant column is the one
whose name contains `workspace` (`workspace_id`, `contributor_workspace_id`, `owner_workspace_id`, …) — and
delete the `workspaces` and `workspace_configs` rows (keyed by `id`) last. Deleting the mapping is what makes
the kept ledger rows unlinkable.

## 4. The other stores

A Lens workspace id is `u` + 26 characters derived from the person's sign-in (issuer and subject,
`deriveWorkspaceID` in `cmd/lens/provision_handler.go`). Track derives its workspace **slug** from the same
sign-in and the same 26 characters with a `w` in front (`bootstrapSlug`, talyvor-track
`internal/workspace/bootstrap.go`). So for Lens workspace `uabc…`, Track's slug is `wabc…`. Track's workspace
**id** is a random UUID, so look it up by slug. Docs keys everything by that Track workspace id, not by the
Lens id.

| Store | What it holds | How to delete it |
|---|---|---|
| **Track**: Postgres `talyvor_track` | about 32 tables: the workspace, its members (by email), issues, comments, embeddings, AI spend | `DELETE /v1/workspaces/{trackWorkspaceID}` on Track with the owner's credential and body `{"confirm":"<slug>"}`. It is restorable for 14 days, after which the hourly purge removes the workspace and every table that references it (talyvor-track `internal/workspace/purge.go`). The suite does not expose this route, so an operator calls Track directly. To erase at once instead of after 14 days, say so in the reply and run the purge's deletes by hand. |
| **Docs**: Postgres `talyvor_docs` | spaces, pages, page versions, blocks, comments, embeddings, views, permissions, share links, approvals, changelog, edit sessions, AI spend, members, library templates, custom domains | Docs has no workspace-level erase. `DELETE /spaces/{spaceID}` removes each space with its pages and their data. After that, delete the rows keyed by the Track workspace id that no space owns: `workspace_members`, `library_templates`, `custom_domains` (`DELETE … WHERE workspace_id = '<trackWorkspaceID>'`). Track's purge does not reach Docs. |
| **Chat history** | every chat conversation's messages, in the person's own browser | localStorage key `talyvor.chat.v1:<identity>` (suite `apps/web/src/areas/chat/history.ts`); no server keeps a copy. We cannot delete it; tell the person to clear site data for the Talyvor app in each browser they used. |
| **Suite BFF** | the signed-in session: identity, email, workspace ids, Lens token | Held in memory only, and gone at sign-out, expiry or restart. There is no database. |
| **Browser, other** | the Docs sidebar's pinned and recent pages, the theme, a pending top-up or plan marker | Also on the person's own machine; clearing site data removes them. |
| **Talyvor Code** (VS Code extension) | API keys in VS Code's secret storage, and `.talyvor/codebase-index.json` and `.talyvor-active-scope` in the customer's own repository | Held only on the customer's machine. They remove the extension's keys and those files. We hold none of it. |

On the server, Lens, Track and Docs run in the compose project in talyvor-suite-deploy's
`deploy/track-docs.compose.yaml` as the
services `lens`, `track` and `docs`, on one `postgres` service. The databases are `talyvor_lens`,
`talyvor_track` and `talyvor_docs`.

## 5. Tell the requester

Reply with the completion time and what was deleted. Say that billing and ledger records are kept, as the
law requires, and that answers already given to other users stay in their conversations.
