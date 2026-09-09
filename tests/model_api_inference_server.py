"""Isolated acceptance provider using actual cached BGE weights, never production."""

import argparse

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

from rag_mcp.embeddings import BgeM3Embedder
from rag_mcp.rerankers.cross_encoder import CrossEncoderReranker
from rag_mcp.contracts import Candidate


def main():
    import uvicorn

    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--device", default="cuda:0")
    args = parser.parse_args()
    embedder = BgeM3Embedder(device=args.device)
    reranker = CrossEncoderReranker(device=args.device)
    app = FastAPI()

    class EmbeddingsBody(BaseModel):
        model: str
        input: list[str]

    class RerankBody(BaseModel):
        model: str
        query: str
        documents: list[str]
        top_n: int

    @app.get("/v1/models")
    def models():
        return {"data": [{"id": "served-embedding"}, {"id": "served-reranker"}]}

    @app.post("/v1/embeddings")
    def embeddings(body: EmbeddingsBody):
        if body.model not in {"served-embedding", "BAAI/bge-m3"}:
            raise HTTPException(status_code=404, detail="Unknown embedding model")
        vectors = embedder.embed_documents(body.input)
        return {"data": [{"index": index, "embedding": vector} for index, vector in enumerate(vectors)]}

    @app.post("/v1/rerank")
    def rerank(body: RerankBody):
        if body.model not in {"served-reranker", "BAAI/bge-reranker-base"}:
            raise HTTPException(status_code=404, detail="Unknown reranker model")
        candidates = [Candidate(str(index), str(index), "", "", text, 0, {})
                      for index, text in enumerate(body.documents)]
        return {"results": [{"index": int(item.document_id), "relevance_score": item.score}
                            for item in reranker.rerank(body.query, candidates)[:body.top_n]]}

    uvicorn.run(app, host="127.0.0.1", port=args.port, log_level="warning")


if __name__ == "__main__":
    main()
