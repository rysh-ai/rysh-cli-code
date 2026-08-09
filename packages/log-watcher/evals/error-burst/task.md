---
until: the report names the first bad line, the burst window, and a root-cause candidate, read-only
---
You are the log-watcher agent. Analyse the log excerpt below exactly as your
skill instructs: first bad line (not the loudest), burst window, counts, one
root-cause candidate. The excerpt is complete — do not run any command, and
do not create, modify, or delete any file.

```
2026-07-30T03:11:02Z api INFO  request served path=/v1/orders dur=42ms
2026-07-30T03:11:09Z api INFO  request served path=/v1/orders dur=38ms
2026-07-30T03:11:41Z db-primary WARN connection_pool saturated (98/100)
2026-07-30T03:11:58Z api INFO  request served path=/v1/orders dur=41ms
2026-07-30T03:12:04Z db-primary ERROR connect timeout after 5s (pool exhausted)
2026-07-30T03:12:05Z api ERROR upstream timeout path=/v1/orders dur=5002ms
2026-07-30T03:12:05Z api ERROR upstream timeout path=/v1/orders dur=5004ms
2026-07-30T03:12:06Z api ERROR upstream timeout path=/v1/orders dur=5001ms
2026-07-30T03:12:07Z worker ERROR job order-sync failed: context deadline exceeded
2026-07-30T03:12:08Z api ERROR upstream timeout path=/v1/orders dur=5003ms
2026-07-30T03:12:11Z db-primary ERROR connect timeout after 5s (pool exhausted)
2026-07-30T03:12:12Z api ERROR upstream timeout path=/v1/orders dur=5000ms
```

Report: what, when, where, and your root-cause candidate.
