# Dockerfile for Market Alert Service
FROM python:3.11-slim

# Set working directory
WORKDIR /app

# Copy application
COPY . /app

# Install dependencies
RUN pip install --no-cache-all hermes-agent

# Create necessary directories
RUN mkdir -p /app/data /app/logs /app/alerts

# Expose ports (if needed for API)
EXPOSE 8000

# Health check
HEALTHCHECK --interval=30s --timeout=30s --start-period=5s --retries=3 \\
    CMD python -c "import json, os; os.path.exists('data')" || exit 1

# Run the service
CMD ["python", "market_alert_service.py"]