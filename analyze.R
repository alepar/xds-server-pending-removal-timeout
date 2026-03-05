#!/usr/bin/env Rscript
# analyze.R — Merge baseline & fixed bench CSVs into a side-by-side comparison
#
# Usage: Rscript analyze.R bench-result-legacy.csv bench-result-fixed.csv
#   First file = baseline, second file = fixed

suppressPackageStartupMessages({
  library(dplyr)
  library(readr)
  library(tidyr)
})

args <- commandArgs(trailingOnly = TRUE)
if (length(args) != 2) {
  cat("Usage: Rscript analyze.R <baseline.csv> <fixed.csv>\n")
  quit(status = 1)
}

read_bench <- function(path) {
  first_line <- readLines(path, n = 1)
  swap_ms <- as.numeric(sub(".*swap_ms=", "", first_line))
  df <- read_csv(path, comment = "#", show_col_types = FALSE)
  list(data = df, swap_ms = swap_ms)
}

summarize_buckets <- function(df, prefix) {
  df |>
    mutate(bucket_s = floor(start_ms / 100) * 100 / 1000) |>
    filter(bucket_s >= 9, bucket_s < 15) |>
    group_by(bucket_s) |>
    summarize(
      "{prefix}_reqs"   := n(),
      "{prefix}_err%"   := sprintf("%.1f", sum(error != "none") / n() * 100),
      "{prefix}_p50_us" := sprintf("%.0f", quantile(duration_us, 0.50)),
      "{prefix}_avg_us" := sprintf("%.0f", mean(duration_us)),
      "{prefix}_p99_us" := sprintf("%.0f", quantile(duration_us, 0.99)),
      .groups = "drop"
    )
}

baseline <- read_bench(args[1])
fixed    <- read_bench(args[2])

cat(sprintf("Baseline swap at %.1fs | Fixed swap at %.1fs\n",
            baseline$swap_ms / 1000, fixed$swap_ms / 1000))
cat(sprintf("Window: 9.0s - 15.0s (covers endpoint removal period)\n\n"))

base_stats  <- summarize_buckets(baseline$data, "base")
fixed_stats <- summarize_buckets(fixed$data, "fix")

merged <- full_join(base_stats, fixed_stats, by = "bucket_s") |>
  arrange(bucket_s) |>
  mutate(
    time = sprintf("%.1f-%.1fs", bucket_s, bucket_s + 0.1),
    sep  = " | "
  ) |>
  select(
    time,
    base_reqs, `base_err%`, base_p50_us, base_avg_us, base_p99_us,
    sep,
    fix_reqs, `fix_err%`, fix_p50_us, fix_avg_us, fix_p99_us
  )

# Mark swap rows
swap_base_bucket <- floor(baseline$swap_ms / 100) * 100 / 1000
swap_fix_bucket  <- floor(fixed$swap_ms / 100) * 100 / 1000

# Print header
header <- sprintf("%-13s %5s %6s %8s %8s %8s  %s  %5s %6s %8s %8s %8s",
  "time", "reqs", "err%", "p50", "avg", "p99",
  "|",
  "reqs", "err%", "p50", "avg", "p99")
cat(sprintf("%13s %-43s  %s  %-43s\n", "", "BASELINE", "|", "FIXED"))
cat(header, "\n")
cat(paste(rep("-", nchar(header)), collapse = ""), "\n")

for (i in seq_len(nrow(merged))) {
  r <- merged[i, ]
  bucket_s <- base_stats$bucket_s[match(r$time, sprintf("%.1f-%.1fs", base_stats$bucket_s, base_stats$bucket_s + 0.1))]

  marker <- ""
  if (!is.na(bucket_s)) {
    if (bucket_s == swap_base_bucket) marker <- paste0(marker, " <-SWAP(B)")
    if (bucket_s == swap_fix_bucket)  marker <- paste0(marker, " <-SWAP(F)")
  }

  cat(sprintf("%-13s %5s %6s %8s %8s %8s  |  %5s %6s %8s %8s %8s%s\n",
    r$time,
    ifelse(is.na(r$base_reqs), "-", as.character(r$base_reqs)),
    ifelse(is.na(r$`base_err%`), "-", r$`base_err%`),
    ifelse(is.na(r$base_p50_us), "-", r$base_p50_us),
    ifelse(is.na(r$base_avg_us), "-", r$base_avg_us),
    ifelse(is.na(r$base_p99_us), "-", r$base_p99_us),
    ifelse(is.na(r$fix_reqs), "-", as.character(r$fix_reqs)),
    ifelse(is.na(r$`fix_err%`), "-", r$`fix_err%`),
    ifelse(is.na(r$fix_p50_us), "-", r$fix_p50_us),
    ifelse(is.na(r$fix_avg_us), "-", r$fix_avg_us),
    ifelse(is.na(r$fix_p99_us), "-", r$fix_p99_us),
    marker))
}

# Summary
cat("\n")
for (label_info in list(list("Baseline", baseline), list("Fixed", fixed))) {
  label <- label_info[[1]]
  bench <- label_info[[2]]
  df <- bench$data
  n_total <- nrow(df)
  n_err <- sum(df$error != "none")
  qps <- n_total / (max(df$start_ms) - min(df$start_ms)) * 1000
  cat(sprintf("%s: %d requests, %.1f%% success, %.0f QPS",
              label, n_total, (n_total - n_err) / n_total * 100, qps))
  if (n_err > 0) {
    err_tbl <- table(df$error[df$error != "none"])
    cat(sprintf(", errors: %d (%s)", n_err,
                paste(names(err_tbl), err_tbl, sep = "=", collapse = ", ")))
  }
  cat("\n")
}
