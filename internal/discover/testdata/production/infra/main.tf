provider "hcloud" {
  token = var.hcloud_token
}

variable "attachments_bucket" {
  type    = string
  default = "bookstore-attachments"
}

variable "prefix" {
  type = string
}

locals {
  exports_bucket = "bookstore-exports"
}

resource "hcloud_server" "node-1" {
  name        = "node-1"
  server_type = "cpx31"
}

resource "hcloud_server" "node-2" {
  name        = "node-2"
  server_type = "cpx31"
}

resource "hcloud_server" "node-3" {
  name        = "node-3"
  server_type = "cpx31"
}

resource "minio_s3_bucket" "crm_attachments" {
  bucket = var.attachments_bucket
}

resource "minio_s3_bucket" "crm_exports" {
  bucket = local.exports_bucket
}

resource "minio_s3_bucket" "crm_reports" {
  bucket = "${var.prefix}-reports"
}
