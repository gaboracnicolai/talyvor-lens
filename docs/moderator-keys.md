# Moderator keys (B20.13)

A moderator key lets the web app run the marketplace review queue without holding Lens admin.
It can do exactly three things:

| Route | What it does |
|---|---|
| `GET /v1/admin/marketplace/review` | held and reported listings, with their reports |
| `POST /v1/admin/marketplace/listings/{id}/approve` | keep a listing up and resolve its reports |
| `POST /v1/admin/marketplace/listings/{id}/takedown` `{"reason": "…"}` | take it down and refund its buyers |

Every other admin route answers **403** to it, and so does every workspace route. A revoked key
answers **401**.

Every request must name the person acting in `X-Talyvor-Operator` (for example the signed-in
operator's email); a request without it answers 400 and does nothing. Each use is written to the
append-only `moderator_key_uses` table (key, operator, method, path, time) before the action runs;
if that write fails the action does not run.

## Creating the key on the server

From the directory holding Lens's `docker-compose.yaml`:

```sh
docker compose exec lens /lens moderator-keys create web app
```

It prints the key once — `tlv_mod_` followed by 48 hex characters. Lens keeps only its sha256 hash, so
copy it now. Put it in the web app's environment file:

```sh
sudo sh -c 'echo "LENS_MODERATOR_KEY=tlv_mod_…" >> /etc/talyvor/bff.env'
```

and restart the web app so it reads it.

## Listing, auditing and revoking

```sh
docker compose exec lens /lens moderator-keys              # every key: id, prefix, name, who made it, state
docker compose exec lens /lens moderator-keys uses <id>    # its last 50 uses: when, operator, route
docker compose exec lens /lens moderator-keys revoke <id>  # 401 from the next request
```

To rotate: create a new key, put it in `/etc/talyvor/bff.env`, restart the web app, then revoke the old one.
