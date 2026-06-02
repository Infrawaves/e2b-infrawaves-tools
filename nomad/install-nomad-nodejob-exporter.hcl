variable "datacenter" {
  type        = string
  description = "Nomad datacenter to run on"
}

variable "node_pool" {
  type        = string
  default     = "default"
  description = "Nomad node pool"
}

variable "version_tag" {
  type        = string
  default     = "manual"
  description = "Tag shown in `nomad job status`, helps tell runs apart"
}

variable "gh_token" {
  type        = string
  default     = ""
  description = "Optional GitHub token to bypass api.github.com 60/h/IP rate limit"
}

variable "script_url" {
  type        = string
  default     = "https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/install-nomad-nodeJob-exporter.sh"
  description = "Install script URL. Override to test branch versions before merging."
}

job "install-nomad-nodejob-exporter" {
  datacenters = [var.datacenter]
  node_pool   = var.node_pool
  type        = "sysbatch"

  meta {
    version = var.version_tag
  }

  # 想只在某台节点上跑就解开下面的 constraint
  # constraint {
  #   attribute = "${node.unique.name}"
  #   value     = "<node-name>"
  # }

  group "install" {
    reschedule {
      attempts  = 0
      unlimited = false
    }

    restart {
      attempts = 0
      mode     = "fail"
    }

    task "run" {
      driver = "raw_exec"

      env {
        GH_TOKEN = var.gh_token
      }

      config {
        command = "/bin/bash"
        args = [
          "-c",
          <<-SCRIPT
          set -uo pipefail
          SCRIPT_URL="${var.script_url}"
          SERVICE="nomad-nodeJob-exporter"
          : "$GH_TOKEN"   # ensure bound under set -u even if empty

          echo "[install] $(date -Iseconds) host=$(hostname) node_ip=$(hostname -I | awk '{print $1}')"

          curl -fsSL "$SCRIPT_URL" | GH_TOKEN="$GH_TOKEN" bash
          rc=$?

          if [ $rc -ne 0 ]; then
            echo "[install] FAILED rc=$rc — dumping diagnostics:"
            echo "----- GitHub API rate limit -----"
            if [ -n "$GH_TOKEN" ]; then
              curl -s -H "Authorization: token $GH_TOKEN" https://api.github.com/rate_limit 2>&1 || true
            else
              curl -s https://api.github.com/rate_limit 2>&1 || true
            fi
            echo
            echo "----- systemctl status -----"
            systemctl status "$SERVICE" --no-pager -l 2>&1 || true
            echo "----- journalctl (last 100) -----"
            journalctl -u "$SERVICE" --no-pager -n 100 2>&1 || true
            echo "----- /metrics probe -----"
            if curl -sf -o /dev/null http://127.0.0.1:9106/metrics; then
              echo "metrics ok"
            else
              echo "metrics unreachable (curl rc=$?)"
            fi
            exit $rc
          fi

          echo "[install] done on $(hostname)"
          SCRIPT
        ]
      }

      resources {
        cpu    = 100
        memory = 128
      }

      logs {
        max_files     = 3
        max_file_size = 10
      }
    }
  }
}
