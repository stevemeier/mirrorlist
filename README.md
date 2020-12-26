# mirrorlist
Mirrorlist implemented in Go

## Overview

This is an implementation for a mirrorlist server, inspired by https://github.com/CentOS/mirrorlists-code

It's written from scratch, in Go with a minimalist approach.

It consists of a frontend part `mirrorlist.go` and a backend part `mirrorlist_updater.go`
Both share a common database backend. Currently, SQLite and MySQL/MariaDB are supported as backends.

## Database structure

The database has three tables:
 * mirrors
 * repos
 * status
 
The `mirrors` table holds all information on mirrors such as hostname, location, supported protocols, etc.
The location information is used to determine which mirrors are closest to a client.

The `repos` table holds the repository definition, each consisting of a major release (e.g. 7, 8), a path,
a name and architecture.

The `status` table binds the two together and keeps track of the timestamp for each repository on each
mirror. This table is used in the mirror selection process to use the most up-to-date mirrors.

## Backend

The backend process `mirrorlist_updater` runs perpetually. When the configurable re-scan interval is reached,
the status of a repository is refreshed to check if it is reachable and up-to-date. This is done by retrieving
the repomd.xml file from each yum-style repository, which holds timestamps in epoch format.
(Example: http://mirror.centos.org/centos/8/BaseOS/x86_64/os/repodata/repomd.xml)

The backend uses Go channels to determine which mirrors/repositories need to be checked, schedule them and
execute tasks in parallel.

## Frontend

The frontend process `mirrorlist` starts a web-server on a configurable port. This can either be exposed
directly to the Internet or used as a backend for Apache/nginx.

For each incoming request, the repository and its mirrors are identified. Depending on the number of mirrors,
the list is narrowed down to nearby servers (based on the client's IP address). The result is cached to improve performance.

The frontend offers multiple endpoints under the `/admin` path to allow cache, repository and mirror management.
These endpoints follow a REST-style logic with HTTP methods such as POST (to create), PATCH (to modify) and
DELETE (to remove) to manage objects.

## Configuration

The default configuration file for both backend and frontend is `mirrorlist.conf`.
An alternative configuration file can be used by specifying `--config <file>` when running each process.

The configuration file can be in either JSON or YAML format, based on the user's preference.

The repository includes a sample configuration file (in JSON format) with all available settings.

## Requirements

To build this software (using `make`) you need a recent version of Go (tested with 1.15.5).

The location matching uses data from MaxMind's GeoLite2 database (see https://dev.maxmind.com/geoip/geoip2/geolite2/).
Due to its license, the file `GeoLite2-City.mmdb` is not included in this repository. Get your own copy :-)

## Status

While this software works, it is still under development and should be considered BETA. PRs are welcome.
