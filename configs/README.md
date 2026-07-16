# Configuration Storage

This directory is reserved for service-specific configuration files that are tracked in version control.

## Guidelines
1. **No Sensitive Data**: Never store plain-text credentials, API keys, or private tokens here. Use the `.env` file or Docker secrets.
2. **Read-Only Mounts**: Prefer mounting configurations as read-only (`:ro`) in your docker-compose files to maintain reproducibility.
3. **Structuring**: Group configs by service name, for example:
   ```
   configs/
   ├── traefik/
   │   └── traefik.yml
   ├── mosquitto/
   │   └── mosquitto.conf
   └── prometheus/
       └── prometheus.yml
   ```
