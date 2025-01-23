FROM python:3.9-slim

WORKDIR /app

COPY requirements.txt .

COPY deposit_bonds.py .

RUN pip install --no-cache-dir -r requirements.txt

CMD ["python", "deposit_bonds.py"]
