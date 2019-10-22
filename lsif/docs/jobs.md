# LSIF worker jobs

The following job types are enqueued into [bull](https://github.com/OptimalBits/bull) and handled by the an instance of the worker process. Each job is enqueued with a `name`, which determines how the job is performed, and an `args` object.

### `convert(repository, commit, filepath)`

Convert an LSIF dump into a SQLite database and add cross-repository information into the cross-repository database (defined packages, imported references, and an LSIF data marker).

Arguments:

- `repository`: the name of the repository from which the LSIF dump was generated
- `commit`: the 40 character commit at which the LSIF dump was generated
- `filepath`: the path on disk where the LSIF upload data can be found. The file contents are assumed to be gzipped, and each line of the file contains a vertex or edge structure encoded as JSON.
