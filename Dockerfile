FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt \
    && groupadd --gid 10001 relay \
    && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin relay \
    && mkdir -p /app/data \
    && chown -R relay:relay /app

COPY --chown=relay:relay app.py admin_api.py admin_ui.py config.py config_store.py db.py limiter.py provider_registry.py runtime.py tenant_store.py upstream.py ./
USER relay
EXPOSE 8000
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
