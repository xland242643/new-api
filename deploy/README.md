# Deployment Templates

Deployment templates in this directory contain no production secrets.

```text
deploy/
└── production/
    ├── docker-compose.yml
    ├── server.env.example
    └── README.md
```

The production server copies these templates into `/srv/server/new-api`.
Real `.env` files, backups, and operational notes belong in the separate
private `NewAPI-ops` repository and must never be committed here.
