import express from "express";
import { createHash } from "crypto";

// Minimal DELTA off-chain service:
// - hashes land-record bundles (7/12 utara + property card) -> recordHash for TitleDeedNFT.mint
// - stubs KYC authorize + registry indexing. Replace with MahaBhulekh + Aadhaar eKYC adapters.

const app = express();
app.use(express.json({ limit: "2mb" }));

// POST /hash-record { surveyNumber, district, areaSqM, documents: string[] } -> { recordHash, propertyId }
app.post("/hash-record", (req, res) => {
  const { surveyNumber, district, areaSqM, documents } = req.body ?? {};
  if (!surveyNumber || !district || !documents) return res.status(400).json({ error: "missing fields" });
  const bundle = JSON.stringify({ surveyNumber, district, areaSqM, documents });
  const recordHash = "0x" + createHash("sha256").update(bundle).digest("hex");
  const propertyId = "0x" + createHash("sha256").update(`${district}|${surveyNumber}`).digest("hex");
  res.json({ recordHash, propertyId, note: "Store bundle off-chain (encrypted). Only hash goes on-chain." });
});

// GET /health
app.get("/health", (_req, res) => res.json({ ok: true, service: "delta-backend" }));

const port = process.env.PORT ?? 8787;
app.listen(port, () => console.log(`delta-backend on :${port}`));
