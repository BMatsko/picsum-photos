# Deploying to Railway

This repo runs as **two separate Railway services** that talk to each other:

| Service | Dockerfile | Purpose |
|---|---|---|
| `picsum-api` | `Dockerfile.picsum` | Public-facing API (`/200/300`, `/seed/abc/200/300`, etc.) |
| `image-service` | `Dockerfile.image-service` | Internal image processor (crop, resize, blur via libvips) |

---

## 1. Create a Railway project

1. Go to [railway.app](https://railway.app) → **New Project → Deploy from GitHub repo**
2. Select `BMatsko/picsum-photos`
3. Railway will create one service — rename it `picsum-api`

---

## 2. Add the image-service

1. In the same project, click **+ New → GitHub Repo** again (same repo)
2. Rename this service `image-service`

---

## 3. Set Dockerfile paths

For each service, go to **Settings → Build → Dockerfile Path**:

- `picsum-api` → `Dockerfile.picsum`
- `image-service` → `Dockerfile.image-service`

---

## 4. Add a Volume for your photos

1. Click **+ New → Volume** in your project
2. Mount it to **both services** at `/data`
   - `picsum-api` needs `/data/metadata.json`
   - `image-service` needs `/data/images/<id>.jpg`

Upload your photos via the Railway volume browser or use the Railway CLI:
```bash
railway volume cp ./your-photos/ <volume-id>:/data/images/
railway volume cp ./metadata.json <volume-id>:/data/metadata.json
```

---

## 5. Set environment variables

### picsum-api

| Variable | Value |
|---|---|
| `PICSUM_IMAGE_SERVICE_URL` | Internal Railway URL of `image-service`, e.g. `https://image-service.railway.internal` |
| `PICSUM_ROOT_URL` | Public URL of `picsum-api`, e.g. `https://picsum-api.up.railway.app` |
| `PICSUM_HMAC_KEY` | A long random secret string (shared with image-service) |
| `PORT` | `8080` (Railway sets this automatically) |

### image-service

| Variable | Value |
|---|---|
| `IMAGE_HMAC_KEY` | Same secret as `PICSUM_HMAC_KEY` above |
| `PORT` | `8081` |

> Generate a strong HMAC key: `openssl rand -hex 32`

---

## 6. Update metadata.json with your photos

Each entry in `metadata.json` maps to a file in `/data/images/`:

```json
[
  {
    "id": "1",
    "author": "Your Name",
    "url": "https://your-site.com",
    "width": 5616,
    "height": 3744
  },
  {
    "id": "2",
    "author": "Your Name",
    "url": "https://your-site.com",
    "width": 4000,
    "height": 3000
  }
]
```

- `id` must match the filename: `"id": "42"` → `/data/images/42.jpg`
- To auto-populate `width`/`height` from real JPEGs, run the manifest tool locally:
  ```bash
  go run ./cmd/image-manifest -image-path ./your-photos -image-manifest-path ./metadata.json
  ```

---

## 7. Seeding reference

| Endpoint | Behavior |
|---|---|
| `/200/300` | Random image, different every request |
| `/seed/abc/200/300` | Always the same image for seed `"abc"` |
| `/id/1/200/300` | Always image with `"id": "1"` |

Seeds are hashed with **murmur3** → deterministic across deploys as long as the image pool order doesn't change.

---

## 8. Deploy

Railway auto-deploys on every push to `main`. Force a redeploy any time from the Railway dashboard → **Deploy → Redeploy**.
