# LDAP Server

## Development of `evaluate_database_init.py`

The Python based script does need a few dependencies which are managed via
`Pipenv`.

### Running the tests

```shell
docker compose run --build --rm -it database-initialized-test
```

### Updating the Python dependencies

The dependencies are managed via `Pipenv` and can be updated as follows:

```shell
docker compose run --build --rm -it database-initialized-test \
    pipenv update
```
