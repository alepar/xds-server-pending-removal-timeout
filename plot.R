#!/usr/bin/env Rscript
# plot.R — Two-facet timeline: error rate + endpoint count for each benchmark run.
#
# Usage: Rscript plot.R bench-result-*.csv [output.png]
# Each CSV has a "# swap_ms=..." header and two sections (requests, endpoints).

if (Sys.getenv("TZ") == "") Sys.setenv(TZ = "UTC")
suppressPackageStartupMessages(library(tidyverse))

args <- commandArgs(trailingOnly = TRUE)
csv_files <- args[!grepl("\\.png$", args)]
out_path  <- args[grepl("\\.png$", args)]
if (length(csv_files) < 1) stop("Usage: Rscript plot.R bench-result-*.csv [output.png]")
if (length(out_path) == 0) out_path <- "bench-plot.png"

bucket_s <- 0.25

read_bench <- function(path) {
  label <- str_extract(basename(path), "(?<=bench-result-).+(?=\\.csv)")
  lines <- readLines(path)
  swap_ms <- as.numeric(str_extract(lines[1], "\\d+"))

  # Split into requests / endpoints sections by header lines
  req_start <- which(lines == "start_ms,duration_us,status")
  ep_start  <- which(lines == "start_ms,count")

  req_lines <- lines[(req_start + 1):(ep_start - 2)]  # skip "# section=endpoints"
  ep_lines  <- lines[(ep_start + 1):length(lines)]

  requests <- read_csv(I(req_lines), col_names = c("start_ms", "duration_us", "status"),
                       col_types = "iii", show_col_types = FALSE) |>
    mutate(label = label, t_s = start_ms / 1000, is_error = status != 200)

  endpoints <- read_csv(I(ep_lines), col_names = c("start_ms", "count"),
                        col_types = "ii", show_col_types = FALSE) |>
    mutate(label = label, t_s = start_ms / 1000)

  list(requests = requests, endpoints = endpoints, swap_s = swap_ms / 1000, label = label)
}

benches <- map(csv_files, read_bench)
swap_s  <- benches[[1]]$swap_s  # assume same warmup across runs

# --- Error rate in buckets ---
errors <- map_dfr(benches, "requests") |>
  mutate(bucket = floor(t_s / bucket_s) * bucket_s) |>
  summarize(error_pct = mean(is_error) * 100, .by = c(label, bucket))

# --- Endpoint counts ---
ep_counts <- map_dfr(benches, "endpoints")

# --- Window around swap ---
win <- c(swap_s - 3, swap_s + 10)

common <- list(
  geom_line(),
  geom_point(size = 1),
  geom_vline(xintercept = swap_s, linetype = "dashed", alpha = 0.5),
  scale_y_continuous(limits = c(0, NA)),
  theme_minimal(base_size = 11)
)

p_errors <- errors |>
  filter(between(bucket, win[1], win[2])) |>
  ggplot(aes(bucket, error_pct, color = label)) +
  common +
  annotate("text", x = swap_s + 0.1, y = Inf, label = "EDS swap",
           vjust = 1.5, hjust = 0, size = 3) +
  labs(y = "Error rate (%)", color = NULL) +
  theme(axis.title.x = element_blank(), legend.position = "top")

p_endpoints <- ep_counts |>
  filter(between(t_s, win[1], win[2])) |>
  ggplot(aes(t_s, count, color = label)) +
  common +
  labs(x = "Time (seconds)", y = "Endpoints in cluster", color = NULL) +
  theme(legend.position = "none")

p <- patchwork::wrap_plots(p_errors, p_endpoints, ncol = 1, heights = c(2, 1))

ggsave(out_path, p, width = 10, height = 6, dpi = 150)
cat(sprintf("Saved %s (%d files, swap at %.1fs)\n", out_path, length(csv_files), swap_s))
