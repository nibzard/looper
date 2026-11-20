#!/bin/bash
# Prints the go test panic budget for this host in minutes.
#
# The internal/looper suite is fsync-bound, so a host under IO pressure
# stretches a slow-but-progressing suite past any fixed budget: the
# 2026-11-20 check run stretched a 765 s suite past 1800 s while the
# /proc/pressure/io "full" average sat near 85 percent, and the fixed
# 30 m budget aborted it mid-progress. The floor is 30 minutes on a
# quiet host. The same anchors testBudget uses (internal/looper/
# loadbudget_test.go) grow it — the one-minute load per CPU above one
# plus 25-percent shares of the PSI "full" IO-stall fraction — capped
# at six times, so a suite that stops making progress still fails
# inside three hours.

set -u

floor=30
cap=180

load="$(awk '{print $1}' /proc/loadavg 2>/dev/null)" || load=""
cpus="$(nproc 2>/dev/null)" || cpus=""
full="$(awk '/^full/ {for (i = 1; i <= NF; i++) if ($i ~ /^avg60=/) {sub(/^avg60=/, "", $i); print $i; exit}}' /proc/pressure/io 2>/dev/null)" || full=""

awk -v avgload="${load:-0}" -v cpus="${cpus:-1}" -v full="${full:-0}" -v floor="$floor" -v cap="$cap" 'BEGIN {
	if (cpus < 1) cpus = 1
	factor = 1
	if ((e = avgload / cpus - 1) > 0) factor += e
	if ((s = full / 25) > 0) factor += s
	if (factor > 6) factor = 6
	m = int(floor * factor)
	if (m < floor) m = floor
	if (m > cap) m = cap
	printf "%d\n", m
}'
