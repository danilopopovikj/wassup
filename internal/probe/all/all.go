// Package all registers every shipped probe. Import it for its side effect.
package all

import (
	_ "github.com/danilopopovikj/wassup/internal/probe/amqp"
	_ "github.com/danilopopovikj/wassup/internal/probe/electric"
	_ "github.com/danilopopovikj/wassup/internal/probe/fixture"
	_ "github.com/danilopopovikj/wassup/internal/probe/gitevents"
	_ "github.com/danilopopovikj/wassup/internal/probe/hatchet"
	_ "github.com/danilopopovikj/wassup/internal/probe/hcloudprobe"
	_ "github.com/danilopopovikj/wassup/internal/probe/k8s"
	_ "github.com/danilopopovikj/wassup/internal/probe/netprobe"
	_ "github.com/danilopopovikj/wassup/internal/probe/pgprobe"
	_ "github.com/danilopopovikj/wassup/internal/probe/redisprobe"
	_ "github.com/danilopopovikj/wassup/internal/probe/s3probe"
	_ "github.com/danilopopovikj/wassup/internal/probe/signoz"
	_ "github.com/danilopopovikj/wassup/internal/probe/stubs"
	_ "github.com/danilopopovikj/wassup/internal/probe/terraform"
)
