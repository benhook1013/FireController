package cli

func managementLocalHelp(group string) []string {
	switch group {
	case "credentials help":
		return []string{"credentials grant --help", "credentials update --help", "credentials revoke --help"}
	case "workers help":
		return []string{"workers create --help", "workers retirement --help", "workers retire --help", "workers list [--include-retired]"}
	default:
		return []string{"reviews policy --help", "reviews policy-configure --help", "reviews execution-configure --help", "reviews start --help", "reviews run-read --help"}
	}
}
