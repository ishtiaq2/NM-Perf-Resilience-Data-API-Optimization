# Interop check: official Connect-ES client → das-edge ingest

Generates a TypeScript/JavaScript client from `das-00-architecture-and-contract/contract/proto`
with the official toolchain (`buf` + `protoc-gen-es`, Connect-ES v2). It then opens a
bidirectional stream to the edge's `das.v1.NodeIngestService/Report` over HTTP/2 without
TLS, with the JSON codec, and checks two things:

1. A full report is acknowledged (`ACTION_OK`), and a delta against an unknown base gets
   `ACTION_RESYNC`.
2. The reported node appears in `GET /api/volatile-data`, mapped to the das-v1 model
   (enum names → `online`/`minor`, int64 strings → numbers).

```sh
npm install && npm run generate          # needs the npm registry; expects ../../../das-00-architecture-and-contract
../../bin/das-edge -nodes 0 -ingest 127.0.0.1:9090 -listen 127.0.0.1:8080 &
node interop.mjs http://127.0.0.1:9090 http://127.0.0.1:8080
```
