package main

// Centralized naming conventions for infrastructure resources.
// All components MUST use these functions instead of inline string concatenation.
// This ensures naming consistency and makes convention changes a single-point edit.

// PHPContainerName returns the Docker container name for a WordPress site.
func PHPContainerName(site string) string {
	return "php_" + site
}

// VolumeName returns the Docker volume name for a WordPress site.
func VolumeName(site string) string {
	return "wp_" + site
}

// WPDatabaseName returns the MySQL database name for a site.
func WPDatabaseName(site string) string {
	return "wp_" + site
}

// WPDatabaseUser returns the MySQL user name for a site.
func WPDatabaseUser(site string) string {
	return "wp_" + site
}

// WPDatabasePass returns the MySQL password for a site.
func WPDatabasePass(site string) string {
	return "pass_" + site
}

// SiteDomain returns the default domain for a site.
func SiteDomain(site, baseDomain string) string {
	return site + "." + baseDomain
}

// CaddyConfFile returns the Caddy snippet filename for a site.
func CaddyConfFile(site string) string {
	return site + ".caddy"
}

// NginxContainerName returns the Docker container name for a site's nginx sidecar.
func NginxContainerName(site string) string {
	return "nginx_" + site
}

// NginxConfFile returns the nginx server block filename for a site.
func NginxConfFile(site string) string {
	return site + ".conf"
}
