# Examples

Four projects, each a directory you can copy whole. Every one of them is compiled by
`scripts/check-examples` on every CI run, so an example that stopped working fails the build
rather than your afternoon.

| Directory | What it declares |
| --- | --- |
| [`network/`](network/) | A VPC network and one subnetwork inside it, ordered by a reference |
| [`storage-bucket/`](storage-bucket/) | A Cloud Storage bucket, versioned, with uniform bucket-level access |
| [`tag-binding/`](tag-binding/) | A resource-manager tag key, a value under it, and that value bound to a project |
| [`module/`](module/) | One module called twice, plus a firewall rule reading an output of it |

## Running one

```bash
cd examples/network
infrena validate                      # compiles; contacts nothing
infrena plan dev --var gcp_project=my-real-project
infrena apply dev --var gcp_project=my-real-project
```

`validate` needs nothing but the plugin. `plan` and `apply` reach Google and need credentials:

```bash
gcloud auth application-default login
```

## Why every example declares `gcp_project` with a `default:`

A provider instance's configuration crosses to the plugin's `Configure`, so infrena resolves
it for every command — including `discover`, which takes no environment and therefore has no
environment-specific value to resolve from. A variable with no `default:` cannot be resolved
there, and infrena refuses to run rather than let the plugin fall back to whatever project
the machine's credentials happen to name.

So the defaults here are placeholders that make the files runnable as committed, not
suggestions. Override them on the command line with `--var`, or edit them.

## What these examples are not

They are shaped to be read, not to be a baseline. There is no state backend, one environment,
and names that would collide the moment two people ran them in the same project. A real
project sets `backend:`, declares the environments it has, and names things after something.
