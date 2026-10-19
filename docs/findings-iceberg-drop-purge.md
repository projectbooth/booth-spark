# Findings: `DROP TABLE … PURGE` fails against Lakekeeper (2026-10-19)

**Status:** investigation. Its fix 1 is ADR 0122 part A (accepted 2026-10-19): booth-spark sets
`rest-catalog-purge=true`, in the first release after the NFL gate and cutover. Reported from the
homelab on booth-spark 0.3.2.
`scala-smoke.jar`'s last statement, `DROP TABLE lakehouse.scala_smoke.runs PURGE`, failed with
`org.apache.iceberg.exceptions.NotFoundException: File does not exist:
s3://lake/warehouse/<uuid>/metadata/snap-<id>.avro`, inside a Spark job (`AllManifestsTable`).

**Reproduced locally**, from both Python and Scala, with the homelab's exact error. The setup:
- Lakekeeper **v0.13.6**, at booth-lakehouse's pinned digest, with Postgres, over S3 (MinIO);
- a warehouse with the storage profile booth-lakehouse sends, and Lakekeeper's default delete profile `hard`;
- Spark from the runtime image (Spark 4.1.3, Iceberg 1.12.0), with booth-spark's catalog settings:
  a REST catalog, `HadoopFileIO` over S3A, and `rest-catalog-purge` unset.

The reproduction files are in the session scratchpad: `purge/docker-compose.yml`, `purge.py` and `purge.scala`.

## What fails

| Statement (same session, after create, inserts and count) | Python | Scala |
|---|---|---|
| `DROP TABLE t` | succeeds | succeeds |
| `DROP TABLE t PURGE` | **fails** (`NotFoundException … snap-….avro`, task failed 4 times, job aborted) whenever Lakekeeper's purge starts before Spark's own file walk ends | **fails**, the same |
| `DROP TABLE t PURGE` with `spark.sql.catalog.<cat>.rest-catalog-purge=true` | succeeds (3 out of 3) | not run (the same code path) |

With Lakekeeper's default 10 s task poll and MinIO, Spark usually wins the race and `PURGE` succeeds.
With a 100 ms poll it failed every time. The homelab is slower: SeaweedFS, and an executor that has
to start for the job. That is enough for Lakekeeper to win there.

## Why

The cause is **Lakekeeper's** handling of a drop request, combined with **Iceberg's** client-side
purge. Spark's own code is not at fault, and neither is the object store. Your guess (a) is right,
and it goes further than you suspected.

1. **What Iceberg 1.12's `SparkCatalog.purgeTable` does** (read from the jar's bytecode). Against a
   REST catalog with `rest-catalog-purge` unset (the default):
   - It sends a plain drop: `catalog.dropTable(id, purge=false)`. The REST client sends that as
     `DELETE …/tables/t` with **no `purgeRequested` parameter**.
   - It checks that the metadata file still exists.
   - It runs `SparkActions.deleteReachableFiles(metadata)` itself. That is a Spark job that reads
     the table's `all_manifests` and the manifest lists, then deletes every file it finds. It logs
     "Set 'rest-catalog-purge' to true to use the REST catalog's capabilities to purge the table".
   - With `rest-catalog-purge=true` it calls `dropTable(id, purge=true)` instead
     (`purgeRequested=true`) and deletes nothing itself.
2. **What Lakekeeper v0.13.6 does with a drop.** On a warehouse with the `hard` delete profile, it
   treats a drop **without** `purgeRequested` as a purge. Measured with raw REST calls:

   | Request | Audit log | Result |
   |---|---|---|
   | `DELETE` with no parameter | `"drop","purge":"true"` | a `tabular_purge` task deleted the table's whole location 6 to 9 s later |
   | `DELETE …?purgeRequested=false` | `"drop"` | files kept |
   | `DELETE …?purgeRequested=true` | `"drop","purge":"true"` | location deleted |

   The Iceberg REST spec's default for `purgeRequested` is false. Lakekeeper's handling of a missing
   parameter is the deviation.
3. **So `PURGE` deletes everything twice, concurrently.**
   - Lakekeeper's purge task removes the location prefix.
   - Spark's `deleteReachableFiles` job is meanwhile still reading manifest lists from that
     location.
   - When Lakekeeper gets there first, Spark's read fails.
   - The failure is not an object-store visibility delay: the files really were deleted, by
     Lakekeeper (its log says "Data at location `s3://…/<uuid>` deleted").
4. **What the other guesses come to:**
   - (b) SeaweedFS's emptied-folder delay plays no part. The object store only changes the timing.
   - (c) Iceberg's client purge is behaving as designed. It just assumes the server didn't purge.

## The state left behind

After the failed `PURGE`:
- The table is **gone from the catalog**: dropped before Spark's walk began.
- Its files are **gone too**: Lakekeeper's purge task deletes the whole location, whatever Spark
  managed first.

Locally, the location had 0 objects and the namespace listed no tables. Nothing stays registered,
and no broken metadata is left behind.

**On the homelab:**
- **The Lakehouse view:** `scala_smoke.runs` should not be listed. The namespace `scala_smoke` stays,
  empty: a drop never removes its namespace.
- **The object store:** `s3://lake/warehouse/<table uuid>/` should be empty or absent. The uuid is
  the one in the error message.
- **Lakekeeper's log** should have `Task of 'tabular_purge' worker exited successfully. Data at
  location 's3://lake/warehouse/<uuid>' deleted.` If that task instead failed (a storage credential
  problem, say), the files under that prefix are orphans. Delete them by hand; the catalog no
  longer refers to them.

## Recommendation for scripts that drop lakehouse tables from Spark (the NFL gate scripts included)

- **Plain `DROP TABLE` is safe.** It doesn't fail, and on this Lakekeeper it also deletes the
  table's files, asynchronously, a few seconds later. That makes it a purge in effect, not a
  "forget the table, keep the data" drop.
  - **No undrop:** the delete profile is `hard`, so nothing brings a dropped table back. Back up
    first if the data matters.
  - **Re-creating at once:** a table re-created under the same name right after a drop gets a new
    uuid and location, so it doesn't collide with the purge.
- **Avoid `DROP TABLE … PURGE` until the fix ships.** It adds nothing (Lakekeeper purges anyway),
  and it fails intermittently. When it fails, the table is still dropped and its files still go, so
  a script that treats the error as fatal fails for nothing.
- **PyIceberg and other clients** behave the same way: a drop without purge is still a purge on this
  Lakekeeper.

## The fix (for later, not now)

These don't touch the homelab, and nothing ships before the NFL gate and the cutover (ADR 0121's
freeze).

1. **booth-spark:** set `spark.sql.catalog.lakehouse.rest-catalog-purge=true` in `DataConf`.
   - This is one line. `PURGE` is then a server purge, and nothing races (3 out of 3 here).
   - It's for the release after the freeze, with an Integration step covering create, insert,
     `DROP … PURGE`, `DROP`, and the location empty afterwards.
2. **booth-lakehouse, a design question for its owner:** whether a plain drop should keep data.
   Options:
   - always send `purgeRequested=false` through its proxy for non-purge drops;
   - use Lakekeeper's `soft` delete profile, which gives an undrop window.

   That changes what a plain `DROP TABLE` means for every client, so it needs a ruling. It isn't a
   bug fix.
3. **Upstream:** worth reporting to Lakekeeper that a missing `purgeRequested` is treated as true,
   while the REST spec's default is false.
