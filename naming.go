package main

// Centralized naming conventions for infrastructure resources.
// All components MUST use these functions instead of inline string concatenation.
// This ensures naming consistency and makes convention changes a single-point edit.

// PHPContainerName returns the Docker container name for a WordPress site.
func PHPContainerName(site string) string {
	return "php_" + site
}

// StaticContainerName returns the Docker container name for a static site.
func StaticContainerName(site string) string {
	return "static_" + site
}

// VolumeName returns the Docker volume name for a WordPress site.
func VolumeName(site string) string {
	return "vol_" + site
}

// StaticVolumeName returns the Docker volume name for a static site.
func StaticVolumeName(site string) string {
	return "static_vol_" + site
}

// WPDatabaseName returns the MySQL database name for a site.
func WPDatabaseName(site string) string {
	return "wp_" + site
}

// WPDatabaseUser returns the MySQL user name for a site.
func WPDatabaseUser(site string) string {
	return "u_" + site
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

// ContainerNameForType returns the appropriate container name based on job type.
func ContainerNameForType(site string, jobType JobType) string {
	if jobType == JobStaticProvision {
		return StaticContainerName(site)
	}
	return PHPContainerName(site)
}

// TmpUploadContainer returns the name for a temporary upload container.
func TmpUploadContainer(volumeName string) string {
	return "tmp_upload_" + volumeName
}
