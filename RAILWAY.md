# Deploying to Railway

This repo runs as **two Railway services** plus a **PostgreSQL database**:

| Service | Dockerfile | Purpose |
|---|---|---|
| `picsum-api` | `Dockerfile.picsum` | Public API — `/200/300`, `/seed/abc/200/300`, `/id/1/200/300` |
| `image-service` | `Dockerfile.image-service` | Internal image processor (crop, resize, blur via libvips) |
| Postgres | Railway plugin | Stores image metadata (id, author, url, width, height) |

---

## 1. Create a Railway project

1. Go to [railway.app](https://railway.app) → **New Project → Deploy from GitHub repo**
2. Select `BMatsko/picsum-photos`
3. Railway creates one service — rename it `picsum-api`

---

## 2. Add the image-service

1. In the same project, **+ New → GitHub Repo** (same repo)
2. Rename this service `image-service`

---

## 3. Set Dockerfile paths

For each service go to **Settings → Build → Dockerfile Path**:

- `picsum-api` → `Dockerfile.picsum`
- `image-service` → `Dockerfile.image-service`

---

## 4. Add PostgreSQL

1. In your project, **+ New → Database → PostgreSQL**
2. Railway provisions a Postgres instance and automatically injects `DATABASE_URL` into **all services in the project**
3. `picsum-api` will pick it up automatically on startup — no extra config needed

The app creates the `images` table on first boot via `CREATE TABLE IF NOT EXISTS`.

---

## 5. Add a Volume for photo files

The `image-service` needs your JPEG files on disk.

1. **+ New → Volume** → mount to `image-service` at `/data`
2. Upload your photos via the Railway volume browser or CLI:
   ```bash
   railway volume cp ./your-photos/ <volume-id>:/data/images/
   ```

Files must be named `<id>.jpg` matching the `id` column in Postgres (e.g. `1.jpg`, `2.jpg`).

---

## 6. Set environment variables

### picsum-api

| Variable | Value |
|---|---|
| `PICSUM_IMAGE_SERVICE_URL` | Internal URL of `image-service`, e.g. `https://image-service.railway.internal` |
| `PICSUM_ROOT_URL` | Public URL of `picsum-api`, e.g. `https://picsum-api.up.railway.app` |
| `PICSUM_HMAC_KEY` | A long random secret (shared with image-service) |

> `DATABASE_URL` is injected automatically by Railway — do not set it manually.

### image-service

| Variable | Value |
|---|---|
| `IMAGE_HMAC_KEY` | Same secret as `PICSUM_HMAC_KEY` |

Generate a strong HMAC key:
```bash
openssl rand -hex 32
```

---

## 7. Seed your photo database

Once `picsum-api` is running, insert rows into the `images` table via the Railway Postgres shell (**Database → Connect → psql**):

```sql
INSERT INTO images (id, author, url, width, height) VALUES
  ('1', 'Your Name', 'https://your-site.com', 5616, 3744),
  ('2', 'Your Name', 'https://your-site.com', 4000, 3000);
```

Each `id` must match a file in `/data/images/` on the `image-service` volume (e.g. `1.jpg`).

To auto-detect real dimensions from JPEGs locally before inserting:
```bash
go run ./cmd/image-manifest -image-path ./your-photos -image-manifest-path ./metadata.json
```
Then read the JSON output for the correct width/height values.

---

## 8. Seeding reference

| Endpoint | Behaviour |
|---|---|
| `/200/300` | Random image, different every request |
| `/seed/abc/200/300` | Always the same image for seed `"abc"` (murmur3 hash → deterministic) |
| `/id/1/200/300` | Always the image with `id = '1'` |

Seeds are stable as long as the **order of rows** (sorted by `id`) doesn't change.

---

## 9. Deploy

Railway auto-deploys on every push to `main`. Force a redeploy any time from **Deployments → Redeploy**.
