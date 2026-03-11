#!/bin/bash
set -e

# Wait for primary to be ready
until PGPASSWORD=replicator pg_isready -h primary -p 5432 -U postgres; do
  echo "Waiting for primary..."
  sleep 1
done

# Clean data directory and create base backup
rm -rf /var/lib/postgresql/data/*
PGPASSWORD=replicator pg_basebackup -h primary -p 5432 -U replicator -D /var/lib/postgresql/data -Fp -Xs -P -R

# Ensure proper permissions
chown -R postgres:postgres /var/lib/postgresql/data
chmod 700 /var/lib/postgresql/data

# Start postgres as postgres user
exec su-exec postgres postgres -c hot_standby=on
