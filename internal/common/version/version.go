package version

var (
	Version = "0.1.0-dev"
	Commit  = "dev"
)

func Full() string {
	return Version + "+" + Commit
}
