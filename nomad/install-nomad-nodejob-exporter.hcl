variable "datacenter" {
  type = string
}

variable "node_pool" {
  type    = string
  default = "default"
}

variable "version_tag" {
  type    = string
  default = "manual"
}

variable "gh_token" {
  type        = string
  default     = ""
  description = "Optional GitHub token to bypass api.github.com 60/h/IP rate limit"
}

variable "script_url" {
  type    = string
  default = "https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/install-nomad-nodeJob-exporter.sh"
}

job "install-nomad-nodejob-exporter" {
  datacenters = [var.datacenter]
  node_pool   = var.node_pool
  type        = "sysbatch"

  meta {
    version = var.version_tag
  }

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
        args    = ["-c", "curl -fsSL ${var.script_url} | bash"]
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
