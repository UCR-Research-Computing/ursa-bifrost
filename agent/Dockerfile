# ursa-agent (Gemini agent: chat page + A2A) for Cloud Run.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.8 /uv /usr/local/bin/uv
WORKDIR /app
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy PYTHONUNBUFFERED=1
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project
COPY ursa_agent ./ursa_agent
RUN uv sync --frozen --no-dev
RUN useradd -r -u 10001 agent
USER agent
ENV PORT=8080
EXPOSE 8080
CMD ["sh", "-c", "exec /app/.venv/bin/uvicorn ursa_agent.web:app --host 0.0.0.0 --port ${PORT} --proxy-headers --forwarded-allow-ips='*'"]
