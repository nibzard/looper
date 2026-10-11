# Measuretwice adapter

This adapter runs task acceptance checks through Measuretwice 0.1.x. It uses
`jev-1.13.0` through `@typesafe-ai/sdk` 0.6.0. The profile stays unvalidated.
All calls use shadow mode.

Install a built Measuretwice package and the adapter dependencies. For a local
Measuretwice checkout, first follow that project's build instructions. Then run:

```sh
cd adapters/measuretwice
npm install --save-exact /absolute/path/to/measuretwice/packages/measuretwice
npm test
```

The native Measuretwice binding must match your system. Alternatively,
`MEASURETWICE_MODULE` can name the absolute path to a built `dist/index.js`.
This is useful for local development without installing a package.

Configure the adapter from your project:

```sh
export TYPESAFE_API_KEY=YOUR_KEY
export LOOPER_ASSESS_COMMAND='node /absolute/path/to/looper/adapters/measuretwice/assess.mjs'
looper
```

Use your normal secret-management method for the key. Looper does not put it in
the evidence packet or requested-model records.

Looper supplies `LOOPER_EVIDENCE_FILE` and `LOOPER_ASSESS_REPORT`. The packet
contains the immutable task contract, base revision, candidate revision, actual
patch, worker report, and verification output. The adapter writes the full run
report, check definition, profile, and qualification status.

Packets larger than 24,000 UTF-8 bytes return `review` before a provider call.
This leaves room within Jev's input limit. The adapter does not truncate code and
then claim to have assessed the full change. Split large tasks or inspect their
full evidence during product review.

`fail` and `review` request an independent product review. They do not directly
block publication. Errors are recorded separately. An exploration profile cannot
establish false-accept rates or qualify this check for enforcement. The four
scripted tests only verify outcome mapping and size handling. Collect real task
cases and independent labels before relying on semantic judgments as gates.
