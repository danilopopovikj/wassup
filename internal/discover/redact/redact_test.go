package redact

import (
	"strings"
	"testing"
)

func TestCredentialName(t *testing.T) {
	for _, n := range []string{"POSTGRES_PASSWORD", "CLICKHOUSE_DSN", "HATCHET_CLIENT_TOKEN", "S3_ACCESS_KEY", "ELECTRIC_SECRET", "AWS_CREDENTIALS", "DB_PWD", "--password", "basic-auth"} {
		if !CredentialName(n) {
			t.Errorf("%s should look like a credential", n)
		}
	}
	for _, n := range []string{"POSTGRES_SERVER", "CACHE_HOST", "ELECTRIC_URL", "S3_BUCKET", "LOG_LEVEL", "--queues"} {
		if CredentialName(n) {
			t.Errorf("%s should not look like a credential", n)
		}
	}
}

func TestEnvValue(t *testing.T) {
	cases := []struct{ name, value, want string }{
		// a password is never kept, whatever it looks like
		{"POSTGRES_PASSWORD", "s3cr3t-Pg-Pa55", ""},
		{"CLICKHOUSE_PASSWORD", "clickhouse.internal", ""},
		{"DB_PASSWORD", "https://looks-like-a-url.example/x", ""},
		{"TOKEN_ENDPOINT", "https://auth.bookstore.example/oauth/token?client_secret=s3cr3t", "https://auth.bookstore.example"},
		{"API_TOKEN", "tok_live_123", ""},
		// of a URL, the scheme and the host
		{"DATABASE_URL", "postgresql://app:s3cr3t@bookstore-db-rw.bookstore:5432/app?sslmode=require", "postgresql://bookstore-db-rw.bookstore:5432"},
		{"CLICKHOUSE_DSN", "tcp://clickhouse.platform:9000/?database=traces&username=admin&password=s3cr3t", "tcp://clickhouse.platform:9000"},
		{"SLACK_WEBHOOK", "https://hooks.slack.example/services/T000/B000/XXXXXXXX", "https://hooks.slack.example"},
		{"REDIS_URL", "redis://:s3cr3t@redis.bookstore:6379/1", "redis://redis.bookstore:6379/1"},
		{"MONGO_URL", "mongodb://u:p@mongo-0.mongo:27017,mongo-1.mongo:27017/app", "mongodb://mongo-0.mongo:27017"},
		// an @ in the password does not move the host
		{"DATABASE_URL", "postgres://app:p@ss:w0rd@db.bookstore:5432/app", "postgres://db.bookstore:5432"},
		// an @ after a slash has two readings; neither is kept
		{"DATABASE_URL", "postgres://app:p@ss/w0rd@db.bookstore:5432/app", ""},
		{"DATABASE_URL", "postgres://db.bookstore:5432/app?user=app&password=x@evil.example", ""},
		{"REGISTRY_URL", "https://tok_abc/def@registry.bookstore.example", ""},
		{"JDBC_URL", "jdbc:postgresql://db.bookstore:5432/app?user=app&password=s3cr3t", "postgresql://db.bookstore:5432"},
		// keyword connection strings
		{"PG_DSN", "host=db.bookstore port=5432 user=app password=s3cr3t dbname=app", "tcp://db.bookstore:5432"},
		// a host under a name that says address
		{"POSTGRES_SERVER", "bookstore-db-rw", "bookstore-db-rw"},
		{"HATCHET_CLIENT_HOST_PORT", "hatchet-engine.bookstore:7070", "hatchet-engine.bookstore:7070"},
		{"PGHOST", "bookstore-db-rw.bookstore", "bookstore-db-rw.bookstore"},
		{"KAFKA_BROKERS", "kafka-0:9092,kafka-1:9092", "kafka-0:9092,kafka-1:9092"},
		// the settings a binding needs
		{"S3_BUCKET", "bookstore-media", "bookstore-media"},
		{"AWS_REGION", "eu-central-1", "eu-central-1"},
		{"ELECTRIC_REPLICATION_SLOT", "electric_slot_default", "electric_slot_default"},
		// anything else is a name without a value
		{"LOG_LEVEL", "info", ""},
		{"ADMIN_PW", "hunter2hunter2", ""},
		{"FEATURE_FLAGS", "a,b,c", ""},
		// a placeholder is not a host
		{"API_URL", "https://api.${DOMAIN}/v1", ""},
		{"CACHE_HOST", "$(CACHE_SERVICE)", ""},
		{"REDIS_URL", "redis://" + Placeholder + ":6379", ""},
	}
	for _, c := range cases {
		got := EnvValue(c.name, c.value)
		if got != c.want {
			t.Errorf("EnvValue(%s, %q) = %q, want %q", c.name, c.value, got, c.want)
		}
		if again := EnvValue(c.name, got); again != got {
			t.Errorf("EnvValue is not idempotent for %s: %q then %q", c.name, got, again)
		}
	}
}

func TestArgsAndText(t *testing.T) {
	args := Args([]string{"clickhouse-client", "--host", "clickhouse.platform", "--password", "s3cr3t with spaces", "--token=tok_live_123",
		"PGPASSWORD=s3cr3t", "--url", "https://admin:s3cr3t@signoz.platform:8080/api?key=abc", "-Q", "default,exports"})
	got := strings.Join(args, " ")
	for _, secret := range []string{"s3cr3t", "with spaces", "tok_live_123", "key=abc", "admin:"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived in %q", secret, got)
		}
	}
	for _, keep := range []string{"--host clickhouse.platform", "https://signoz.platform:8080", "-Q default,exports", "--password " + Mask} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q is missing from %q", keep, got)
		}
	}
	text := Text(`run --db-password 'my pass' with {"api_key": "abc123"} and token: xyz789 at postgres://u:pw1234@db:5432/x`)
	for _, secret := range []string{"my pass", "abc123", "xyz789", "pw1234"} {
		if strings.Contains(text, secret) {
			t.Errorf("%q survived in %q", secret, text)
		}
	}
	if !strings.Contains(text, "postgres://db:5432") {
		t.Errorf("the host should stay: %q", text)
	}
	if got := Text("module from git::https://deploy:s3cr3t@git.bookstore.example/infra.git?ref=v1"); got != "module from git::https://git.bookstore.example" {
		t.Errorf("a URL behind a prefix: %q", got)
	}
	if got := Args([]string{"git::https://deploy:s3 cr3t@git.bookstore.example/infra.git"}); got[0] != "git::https://git.bookstore.example" {
		t.Errorf("a URL behind a prefix, as an argument: %q", got)
	}
	if plain := "celery -A app worker -Q default,exports -c 4"; Text(plain) != plain {
		t.Errorf("a command without credentials must not change: %q", Text(plain))
	}
}

func TestExpand(t *testing.T) {
	env := map[string]string{"POSTGRES_USER": "app", "POSTGRES_PASSWORD": "s3cr3t", "POSTGRES_DB": "bookstore", "DB_HOST": "bookstore-db-rw"}
	lookup := func(n string) (string, bool) { v, ok := env[n]; return v, ok }
	got := Expand("postgresql://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@$(DB_HOST):5432/$(POSTGRES_DB)", lookup)
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("a credential must not be interpolated: %q", got)
	}
	if want := "postgresql://app:" + Placeholder + "@bookstore-db-rw:5432/bookstore"; got != want {
		t.Errorf("Expand = %q, want %q", got, want)
	}
	if v := EnvValue("DATABASE_URL", got); v != "postgresql://bookstore-db-rw:5432" {
		t.Errorf("the host should be found after interpolation: %q", v)
	}
	// an unknown variable in the place of the host leaves no host
	if v := EnvValue("REDIS_URL", Expand("redis://$(NOWHERE):6379/0", lookup)); v != "" {
		t.Errorf("an unresolved host must not be kept: %q", v)
	}
}

// TestOddPasswords puts passwords with the characters that break URL
// parsers where a password goes and checks that no part of them is kept.
func TestOddPasswords(t *testing.T) {
	for _, pw := range []string{"Zq7/xK2?mN4#vB9", "Zq7@xK2:mN4", "Zq7%xK2%ZZmN4", "Zq7 xK2 mN4", "Zq7)xK2(mN4$", "Zq7&xK2=mN4;", "[Zq7]xK2{mN4}"} {
		var kept []string
		for _, scheme := range []string{"postgresql", "redis", "amqp", "tcp", "clickhouse", "https"} {
			kept = append(kept, EnvValue("DATABASE_URL", scheme+"://admin:"+pw+"@db.bookstore:5432/app"))
			kept = append(kept, EnvValue("STORE_DSN", scheme+"://db.bookstore:9000/app?username=admin&password="+pw))
			kept = append(kept, Text("migrate --url "+scheme+"://admin:"+pw+"@db.bookstore:5432/app --verbose"))
		}
		kept = append(kept, EnvValue("PG_DSN", "host=db.bookstore user=admin password="+pw+" dbname=app"))
		kept = append(kept, EnvValue("DB_PASSWORD", pw), EnvValue("DB_HOST", pw), EnvValue("S3_BUCKET", pw))
		kept = append(kept, strings.Join(Args([]string{"run", "--password", pw, "--secret=" + pw, "DB_PASSWORD=" + pw}), " "))
		for _, k := range kept {
			for _, part := range []string{"Zq7", "xK2", "mN4"} {
				if strings.Contains(k, part) {
					t.Errorf("password %q: %q survived in %q", pw, part, k)
				}
			}
		}
	}
}
