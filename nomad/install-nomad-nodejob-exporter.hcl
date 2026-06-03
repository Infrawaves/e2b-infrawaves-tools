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

variable "nomad_token" {
  type        = string
  default     = ""
  description = "Nomad ACL token for the exporter to call `nomad node status -self`. 通常用 `-var=\"nomad_token=$NOMAD_TOKEN\"` 透传当前 shell 里的 token。"
}

variable "script_url" {
  type        = string
  default     = "https://api.github.com/repos/Infrawaves/e2b-infrawaves-tools/contents/scripts/install-nomad-nodeJob-exporter.sh?ref=main"
  description = "走 api.github.com 而不是 raw.githubusercontent.com,部分内网节点不通 raw"
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
        GH_TOKEN    = var.gh_token
        NOMAD_TOKEN = var.nomad_token
      }

      config {
        command = "/bin/bash"
        args    = ["-c", "curl -fsSL -H 'Accept: application/vnd.github.raw' ${var.script_url} | bash"]
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
