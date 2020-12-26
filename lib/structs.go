package lib

type Location struct {
        Known           bool
        Continent       string
        Country         string
        Region          string
        Longitude       float64
        Latitude        float64
}

type Repo struct {
	ID              int    `json:"id" db:"repo_id"`
	MRelease        int    `json:"release" db:"major_release"`
	Path            string `json:"path" db:"path"`
	Name            string `json:"name" db:"name"`
	Arch            string `json:"arch" db:"arch"`
	Altarch         bool   `json:"is_altarch" db:"is_altarch"`
	Enabled         bool   `json:"enabled" db:"enabled"`
}

type CacheStats struct {
        Entries       int64
        HitCount      int64
        MissCount     int64
        LookupCount   int64
}

type Mirror struct {
	ID          int     `json:"id" db:"mirror_id"`
        Name        string  `json:"name"`
	Basedir     string  `json:"basedir"`
	BasedirAlt  string  `json:"basedir_altarch" db:"basedir_altarch"`
	IPv4        int     `json:"ipv4" db:"ipv4"`
	IPv6        int     `json:"ipv4" db:"ipv6"`
	HTTP        int     `json:"http" db:"http"`
	HTTPS       int     `json:"https" db:"https"`
	Rsync       int     `json:"rsync" db:"rsync"`
	Continent   string  `json:"continent" db:"continent"`
	Country     string  `json:"country" db:"country"`
	Region      string  `json:"region" db:"region"`
	Latitude    float64 `json:"latitude" db:"latitude"`
	Longitude   float64 `json:"longitude" db:"longitude"`
	Enabled     bool    `json:"enabled" db:"enabled"`
}

type Issue struct {
        Name        string          `json:"name"`
        Errors      map[string]int  `json:"errors"`
}

type CheckTask struct {
        MirrorID        int
        RepoID          int
        URL             string
        Iso             bool
        AltArch         bool
        Valid           bool
}

type CheckResult struct {
        MirrorID        int
        RepoID          int
        Timestamp       int64
        Result          int
}
