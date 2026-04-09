# Deploying to Railway

One service. One Dockerfile. Done.

---

## 1. Create a Railway project

1. [railway.app](https://railway.app) → **New Project → Deploy from GitHub repo**
2. Select `BMatsko/picsum-photos`

---

## 2. Add PostgreSQL

1. In your project → **+ New → Database → PostgreSQL**
2. That's it — Railway automatically injects `DATABASE_URL` into your service

The app creates the `images` table on first boot. No migrations to run manually.

---

## 3. Add a Volume for your photos

1. Click your service → **+ New → Volume**
2. Mount path: `/data`

Your JPEG files go in `/data/images/` inside the volume.  
File naming: `1.jpg`, `2.jpg`, etc. — must match the `id` column in the database.

---

## 4. Environment variables

Set these in **your service → Variables tab**:

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | Auto | Injected by Railway Postgres — **do not set manually** |
| `PICSUM_HMAC_KEY` | **Yes** | Any random secret string. Generate with: `openssl rand -hex 32` |
| `PICSUM_ROOT_URL` | No | Your public URL e.g. `https://yourapp.up.railway.app`. Auto-detected from `RAILWAY_PUBLIC_DOMAIN` if not set |
| `PICSUM_STORAGE_PATH` | No | Path to JPEG directory. Defaults to `/data/images` |
| `PORT` | Auto | Injected by Railway — **do not set manually** |

**Minimum required:** just `PICSUM_HMAC_KEY`. Everything else is automatic.

---

## 5. Seed your photos

Once deployed, open the Railway Postgres shell:  
**Database → Connect → psql** (or use any Postgres client with the connection string)

```sql
INSERT INTO images (id, author, url, width, height) VALUES
  ('1', 'Your Name', 'https://your-site.com', 5616, 3744),
  ('2', 'Your Name', 'https://your-site.com', 4000, 3000);
```

Then upload matching JPEGs to `/data/images/` on the Volume.

To auto-detect real dimensions from your JPEGs locally:
```bash
go run ./cmd/image-manifest -image-path ./your-photos -image-manifest-path ./metadata.json
```

---

## 6. URL reference

| URL | Result |
|---|---|
| `/200/300` | Random image, 200×300 |
| `/seed/hello/200/300` | Always the same image for seed `hello` |
| `/id/1/200/300` | Always image with id `1` |
| `/id/1/200/300?grayscale` | Grayscale |
| `/id/1/200/300?blur=5` | Blurred |
| `/v2/list` | JSON list of all images |
| `/id/1/info` | JSON metadata for image `1` |

---

## 7. Redeploy

Railway auto-deploys on every push to `main`.  
Force a manual redeploy: **Deployments → Redeploy**.
