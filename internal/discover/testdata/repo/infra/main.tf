provider "hcloud" {
  token = var.hcloud_token
}

resource "hcloud_server" "node-1" {
  name        = "node-1"
  server_type = "cpx31"
  location    = "fsn1"
}

resource "hcloud_server" "node-2" {
  name        = "node-2"
  server_type = "cpx31"
  location    = "fsn1"
}

resource "hcloud_load_balancer" "main" {
  name               = "bookstore-lb"
  load_balancer_type = "lb11"
  location           = "fsn1"
}

resource "hcloud_load_balancer_target" "nodes" {
  type             = "server"
  load_balancer_id = hcloud_load_balancer.main.id
  server_id        = hcloud_server.node-1.id
}

resource "hcloud_firewall" "main" {
  name = "bookstore"
  rule {
    direction = "in"
    protocol  = "tcp"
    port      = "443"
  }
}

resource "cloudflare_record" "app" {
  name    = "app"
  zone_name = "bookstore.example"
  value   = hcloud_load_balancer.main.ipv4
}

resource "postgresql_replication_slot" "electric" {
  name   = "electric_slot_default"
  plugin = "pgoutput"
}

resource "helm_release" "electric" {
  name      = "electric"
  chart     = "electric-sql/electric"
  namespace = "bookstore"

  set {
    name  = "env.DATABASE_URL"
    value = "postgresql://electric:pw@bookstore-db-rw.bookstore:5432/app"
  }
  set {
    name  = "env.ELECTRIC_SECRET"
    value = "changeme"
  }
}
