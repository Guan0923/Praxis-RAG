# Docling Real Benchmark

Current status: **the full first-phase benchmark completed on 2026-09-07 and is
waiting for user-selected acceptance thresholds**. Read `REPORT.md` for the
measured accuracy, latency, resource use and known failures before rerunning.

This is phase-one measurement tooling, not the production upload pipeline.
Do not delete or migrate existing knowledge stores while running it. Accuracy
and latency acceptance thresholds must be chosen by the user after measurement.

## Isolation

- Application environment: `.venv/app`, prepared after `conda activate dev`.
- Model environment: `.venv/models`, prepared after `conda activate dl`.
- Both are project-local environments. Shared environment packages are unchanged.
- Downloaded documents, weights, vectors, logs and measurements belong under
  ignored `.mini_agent-rag/docling-benchmark/`, not in Git.
- Fixed initial selection: Docling 2.126.0, Heron, EasyOCR `ch_sim`/`en`,
  TableFormer accurate, CodeFormulaV2, BGE-M3 and the existing BGE reranker.
- Imports and retrieval must run in separate processes, both on CUDA. One import
  job runs at a time. No automatic CPU or serial fallback is permitted.

## Public Sources

`corpus.json` fixes repository samples to their source commits and the technical
report to its arXiv version. Some samples are public regression documents rather
than representative business publications; report that limitation explicitly.
Do not derive expected answers from Docling's own extraction output.

```powershell
conda activate dl
.venv\models\Scripts\python.exe benchmarks/docling/prepare.py
```

The downloader enforces the existing 10 MiB file limit. Original files stay
unchanged and remain available for independent visual checks.

## Required Measurements

Use eight PDF/DOCX files, twenty independently checked source locations and
thirty source-grounded retrieval questions. Measure cold loading separately from
warmed conversion/search. Collect per-document times, peak memory and GPU use,
and 100 warmed search requests both without and with concurrent import. Include
the import embedding stage, not only PDF conversion. Record P50/P95, errors,
resource use and correct-evidence recall without inventing pass thresholds.

## Reproduction On This Workstation

Run from the `docling-upgrade` worktree, not `main`. The existing `dl` base used
Python 3.11.15, Torch 2.11.0+cu128 and torchvision 0.26.0+cu128. Do not upgrade that
shared base. These venvs inherit its packages; they are not hermetic containers.
Active package versions and paths are recorded in `environment/active-runtime.json`.

Prepare environments only if absent; do not recreate an environment in use:

```powershell
conda activate dev
python -m venv --system-site-packages .venv/app
.venv/app/Scripts/python.exe -m pip install -e .
conda activate dl
python -m venv --system-site-packages .venv/models
.venv/models/Scripts/python.exe -m pip install -r benchmarks/docling/requirements.txt
```

Download pinned sources and models, then prepare independent source evidence:

```powershell
conda activate dl
$env:PYTHONPATH = Join-Path $PWD 'src'
$env:PYTHONIOENCODING = 'utf-8'
$env:HF_HUB_OFFLINE = '0'
$env:TRANSFORMERS_OFFLINE = '0'
$env:HF_HUB_DISABLE_XET = '1'
.venv/models/Scripts/python.exe benchmarks/docling/prepare.py
.venv/models/Scripts/python.exe benchmarks/docling/prepare_models.py
.venv/models/Scripts/python.exe benchmarks/docling/inventory.py
.venv/models/Scripts/python.exe benchmarks/docling/inspect_sources.py
```

The user-approved cache correction changes only in-memory formula generation
defaults. It does not edit shared libraries or model files. Verify real cache
reuse with the previously stalled PDF, then run the full conversion and review:

```powershell
.venv/models/Scripts/python.exe benchmarks/docling/verify_cache.py
.venv/models/Scripts/python.exe benchmarks/docling/convert.py
.venv/models/Scripts/python.exe benchmarks/docling/review.py
```

`convert.py` defaults to `conversion-cached`, so the original interrupted
`conversion` directory remains unchanged. It records a document after conversion, export and chunking. A process
kill can leave a partial report without `finished_at`; that is not success. The
initial interrupted report and formula sampling remain in `conversion/`.
Use `--run-name <new-name>` for diagnostic conversions; by default the full
benchmark reads `conversion-cached/<sample>/chunks.json`. Pass the same
`--conversion-run` value to `review.py` and `benchmark.py` when changing it.

Use only the dedicated test Qdrant container. The existing stopped test container
can be started with `docker start xinghe-docling-bench-bd6ed176`. Docker assigns
a loopback port when it starts, so read it with `docker port` instead of reusing
an older port. Verify its label `codex.worktree=docling-upgrade` and storage mount first.
The measured image was
`sha256:057ee3a8da769fe7310dd3537b4dc7583bf87a95ce8ac43c0af5a46bc580d1fc`
(Qdrant 1.19.0). Do not point the benchmark at a shared production instance.

After all eight conversions have succeeded:

```powershell
$qdrantPort = (docker port xinghe-docling-bench-bd6ed176 6333/tcp).Split(':')[-1]
.venv/models/Scripts/python.exe benchmarks/docling/benchmark.py --qdrant-url "http://127.0.0.1:$qdrantPort"
docker stop xinghe-docling-bench-bd6ed176
```

Each run uses unique reader/staging collections and its own `runs/<id>` output.
It is an IPC/model/storage benchmark, not a production HTTP upload benchmark.
The importer stays loaded but idle during baseline queries. During conversion it
repeats real documents sequentially; during embedding it generates real BGE-M3
vectors and writes Qdrant. It stops each phase at a document boundary.

`questions.json` contains thirty source-derived questions; `question_sources.json`
gives their original locations. The automatic metric requires the expected source
and normalized terms together in a top-five chunk. It is a reproducible evidence
proxy, not semantic answer accuracy. Review wrong and right hits against original
pages before setting thresholds. `observations.json` records only comparisons
actually performed, separately from generated review panels and term checks.

The completed reference run is
`.mini_agent-rag/docling-benchmark/runs/20260907_191557_6a16aa/`. It measured 100
baseline searches, 50 searches during real Docling conversion and 50 during real
BGE-M3 embedding plus Qdrant writes. The test container was stopped after the run;
start it and re-read its dynamic port before repeating the benchmark.
