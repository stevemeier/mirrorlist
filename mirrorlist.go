package main

import "fmt"
import "log"
import "net"
import "net/http"
import "regexp"

// GeoIP dependencies
import "github.com/oschwald/geoip2-golang"

// SQL dependencies
import _ "github.com/mattn/go-sqlite3"
import _ "github.com/go-sql-driver/mysql"

// Used in /admin endpoints
import "encoding/json"

// For repository checking
import "strconv"
import "time"

// Easier DB handling
import "github.com/jmoiron/sqlx"

// In-Memory caching
import "github.com/coocood/freecache"

// HTTP performance
import "github.com/valyala/fasthttp"

// HTTP routing
import "github.com/fasthttp/router"

// Custom structs and functions
import lib "github.com/stevemeier/mirrorlist/lib"

// Database and Cache handles are global
var geodb *geoip2.Reader
var mirrordb *sqlx.DB
var rescache *freecache.Cache

// Global variables
var listsize int
var dbtype string
var caching bool
var headers map[string]string

// Main
func main() {
  var err error

  // Read config, file does not have to exists. YAML and JSON are supported
  log.Printf("Configuration file is %s\n", lib.Config_path(`mirrorlist.conf`))
  cfg, loaded := lib.Load_config(lib.Config_path(`mirrorlist.conf`))
  if loaded {
    log.Println("Successfully loaded configuration")
  } else {
    log.Println("No configuration loaded, using defaults")
  }

  // Open the GeoLite database
  geodbfile := cfg.UString(`geo-database.file`,`GeoLite2-City.mmdb`)
  log.Printf("Opening GEO database %s\n", geodbfile)
  geodb, err = geoip2.Open(geodbfile)
  if err != nil {
    log.Fatal(err)
  }
  defer geodb.Close()
  check_geodb_age()

  // Configure list size (number of mirrors in each response)
  listsize = cfg.UInt(`frontend.results`, 10)

  // Configure headers
  headers = make(map[string]string)
  headersmap, _ := cfg.Map(`frontend.headers`)
  for k, v := range(headersmap) {
    log.Printf("Setting header \"%s\" to \"%s\"\n", k, convert_interface(v))
    headers[k] = convert_interface(v)
  }

  // Build DSN from config
  driver, dsn := lib.Build_DSN(cfg)
  log.Printf("Using %s with DSN %s\n", driver, dsn)

  // Connect to database
  dbtype = driver
  mirrordb, err = sqlx.Open(driver, dsn)
  if err != nil {
    log.Fatal(err)
  }
  defer mirrordb.Close()

  // Check database status
  tablecount := lib.TableCount(mirrordb, cfg.UString(`database.name`,`mirrorlist`))
  log.Printf("Database has %d tables\n", tablecount)

  // Init Database, if empty
  if (tablecount < 3) {
    log.Println("Initializing database")
    success := lib.InitDatabase(mirrordb)
    if success {
      log.Printf("Initialized database successfully")
    }
  }

  // Read cache configuration from config (default here is true, for performance)
  caching = cfg.UBool(`frontend.cache.enabled`, true)

  // Initialise the cache
  // It's theoretic maximum size is about 850 * repos * ~700 bytes
  // 850 is the number of possible locations based on GeoIP
  // 700 is the average size of a response
  // This needs to be multiplied by the number of repositories
  // Should fit comfortably into 64 Megs
  //
  // Caching reduces processing time from ~5 ms to around ~200 us, around -90%
  if caching {
    log.Println("Initializing cache")
    rescache = freecache.NewCache(cfg.UInt(`frontend.cache.size`,64000000))
  }

  // Set up http paths
  routes := router.New()

  // Public endpoint, always active
  routes.GET("/", http_handler_root)

  // Register admin endpoints if enabled in configuration
  if cfg.UBool(`frontend.admin.read`) {
     log.Println("Enabling HTTP /admin read-only endpoints")
     // Location
     routes.GET("/admin/location", http_handler_location)
     // Cache
     routes.GET("/admin/cache", http_handler_cache_get)
     // Mirrors
     routes.GET("/admin/mirrors", http_handler_mirror_get)
     // Repos
     routes.GET("/admin/repos", http_handler_repo_get)
     // Operations
     routes.GET("/admin/issues", http_handler_issues)
  }

  // Register admin endpoints which permit changes, depending on configuration
  if cfg.UBool(`frontend.admin.write`) {
     log.Println("Enabling HTTP /admin writable endpoints")
     // Cache
     routes.DELETE("/admin/cache", http_handler_cache_delete)
     // Mirror
     routes.POST("/admin/mirrors", http_handler_mirror_post)
     routes.PATCH("/admin/mirrors/{name}", http_handler_mirror_patch)
     routes.DELETE("/admin/mirrors/{name}", http_handler_mirror_delete)
     // Repos
     routes.POST("/admin/repos", http_handler_repo_post)
     routes.PATCH("/admin/repos/{id}", http_handler_repo_patch)
     routes.DELETE("/admin/repos/{id}", http_handler_repo_delete)
  }

  // Start the web server
  log.Printf("Starting HTTP server on %s\n", cfg.UString(`frontend.listen`,`0.0.0.0:8000`))
  laserr := fasthttp.ListenAndServe(cfg.UString(`frontend.listen`,`0.0.0.0:8000`), routes.Handler)
  if laserr != nil {
    log.Fatal(laserr)
  }
}

func get_ip_location (ip string) (lib.Location) {
  var result lib.Location

  record, err := geodb.City(net.ParseIP(ip))
  if err != nil { result.Known = false; return result }

  result.Known = true
  result.Continent = record.Continent.Code
  result.Country = record.Country.IsoCode
  if len(record.Subdivisions) > 0 {
    result.Region = record.Subdivisions[0].IsoCode
  }
  result.Longitude = record.Location.Longitude
  result.Latitude = record.Location.Latitude

  return result
}

func http_handler_location (ctx *fasthttp.RequestCtx) {
  ip := string(ctx.QueryArgs().Peek("ip"))

  if ip == `` {
    ctx.SetStatusCode(http.StatusBadRequest)
    _, werr := ctx.Write([]byte("Parameter \"ip\" not set"))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  result, err := json.Marshal(get_ip_location(ip))
  if err != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(err.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  ctx.Response.Header.Set("Content-Type", "application/json")
  _, werr := ctx.Write(result)
  if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
}

func http_handler_root (ctx *fasthttp.RequestCtx) {
  // Start the clock to measure response time
  start := time.Now()

  // Use remote address or `ip` parameter, if provided
  var clientip string = ctx.RemoteAddr().String()
  if string(ctx.QueryArgs().Peek("ip")) != "" {
    clientip = string(ctx.QueryArgs().Peek("ip"))
  }

  // Determine IPv4 / IPv6
  ipversion := lib.IPversion(clientip)

  // Set headers (configured in frontend.headers)
  for header, value := range(headers) {
    ctx.Response.Header.Set(header, value)
  }

  // Check for required parameters
  // The CentOS version always produces 200 OK
  // We produce 400 Bad Request instead, so that we can find it in the logs
  for _, key := range []string{"arch", "release", "repo"} {
    if (string(ctx.QueryArgs().Peek(key)) == "") {
      ctx.SetStatusCode(http.StatusBadRequest)
      _, werr := ctx.Write([]byte(fmt.Sprintf("%s not specified\n", key)))
      if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
      return
    }
  }

  // Check for a matching repo
  repoid, repopath, is_altarch := get_repo_id(string(ctx.QueryArgs().Peek("release")),
                                              string(ctx.QueryArgs().Peek("repo")),
					      string(ctx.QueryArgs().Peek("arch")) )

  // repoid is an auto_increment field, so its value is at least 1
  if repoid <= 0 {
    ctx.SetStatusCode(http.StatusNotFound)
    _, werr := ctx.Write([]byte("Invalid release/repo/arch combination\n"))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Get client location
  // Caching this would increase performance by about 10% but eats a bunch of RAM, not worth it
  clientloc := get_ip_location(clientip)

  // Check cache for ready-to-send response
  if (caching) {
    // The key for the cache consist of repository ID and the client's location
    // This way a client from the same location asking for the same repository will get the same answer
    response, cachehit := rescache.Get([]byte(fmt.Sprintf("%d%s%s%s%s", repoid, ipversion, clientloc.Continent, clientloc.Country, clientloc.Region)))

    ctx.Response.Header.Set("X-Cache-Hit", strconv.FormatBool(cachehit == nil))
    if cachehit == nil {
      ctx.Response.Header.Set("X-Processing-Time", time.Since(start).String() )
      _, werr := ctx.Write(response)
      if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
      return
    }
  }

  // Find mirrors with the repo
  // Returns a slice of int with matching mirror IDs
  allmirrors := mirrors_with_repo(repoid)
  if len(allmirrors) == 0 {
    log.Printf("Found no mirrors for repo ID %d\n", repoid)
    ctx.SetStatusCode(http.StatusNotFound)
    return
  }

  // Pick local servers, if we have more than we need
  // Returns a sorted slice of int suitable for the client
  mirrors := allmirrors
  if len(allmirrors) > listsize {
    mirrors = nearby_mirrors(clientloc, lib.IPversion(clientip), allmirrors, listsize)
  }

  // Warn if we don't have enough mirrors
  if len(mirrors) < listsize {
    log.Printf("Client %s has only %d mirror(s) available\n", clientip, len(mirrors))
  }

  // Log runtime to header
  ctx.Response.Header.Set("X-Processing-Time", time.Since(start).String() )

  // Write out server list
  // This takes the mirror list ([]int) and the repository information and builds full URLs
  response := full_mirror_urls(mirrors,
                               repopath,
			       string(ctx.QueryArgs().Peek("repo")),
			       string(ctx.QueryArgs().Peek("arch")),
			       is_altarch)

  // Send response to client
  _, werr := ctx.Write(response)
  if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }

  // Add response to cache, if enabled
  // Empty responses are possible, but we don't cache them because they are not useful
  cachekey := fmt.Sprintf("%d%s%s%s%s", repoid, ipversion, clientloc.Continent, clientloc.Country, clientloc.Region)
  if caching && len(response) > 0 {
    cacheerr := rescache.Set([]byte(cachekey), response, 3600)
    if cacheerr != nil {
      log.Printf("Failed to add entry to cache with key %s\n", cachekey)
    }
  }
}

func nearby_mirrors (loc lib.Location, ipversion string, mirrors []int, limit int) ([]int) {
  var result []int

  var random string = lib.DB_Random(dbtype)

  // FIXME: ipversion should probably go into ORDER BY to prefer ipversion but not limit it
  q, args, err := sqlx.In("WITH "+
    "eligible AS (SELECT mirror_id, continent, country, region, ipv4, ipv6, enabled FROM mirrors WHERE mirror_id IN (?)) "+
    "SELECT mirror_id, '3' AS prio, "+random+" AS rand FROM eligible WHERE continent = ? "+
      "AND ipv"+ipversion+" > 0 AND enabled > 0 UNION "+
    "SELECT mirror_id, '2' AS prio, "+random+" AS rand FROM eligible WHERE continent = ? AND country = ? "+
      "AND ipv"+ipversion+" > 0 AND enabled > 0 UNION "+
    "SELECT mirror_id, '1' AS prio, "+random+" AS rand FROM eligible WHERE continent = ? AND country = ? AND region = ? "+
      "AND ipv"+ipversion+" > 0 AND enabled > 0 "+
      "ORDER BY prio, rand ASC LIMIT ?",
      mirrors,
      loc.Continent,
      loc.Continent, loc.Country,
      loc.Continent, loc.Country, loc.Region,
      limit )

  if err != nil {
    log.Printf("nearby_mirrors_int -> prepare -> %s\n", err)
    return result
  }

  var id int
  var prio int
  var rand int64
  rows, err := mirrordb.Query(q,args...)
  if err != nil {
    log.Println(err)
    return result
  }
  defer rows.Close()

  for rows.Next() {
    _ = rows.Scan(&id, &prio, &rand)
    result = append(result, id)
  }

  return result
}

func http_handler_cache_delete (ctx *fasthttp.RequestCtx) {
  if !caching {
    ctx.SetStatusCode(http.StatusNotFound)
    return
  }

  rescache.Clear()
  log.Print("Cache flushed")
  ctx.SetStatusCode(http.StatusNoContent)
}

func http_handler_cache_get (ctx *fasthttp.RequestCtx) {
  if !caching {
    ctx.SetStatusCode(http.StatusNotFound)
    return
  }

  var cs lib.CacheStats
  cs.Entries = rescache.EntryCount()
  cs.HitCount = rescache.HitCount()
  cs.MissCount = rescache.MissCount()
  cs.LookupCount = rescache.LookupCount()

  result, err := json.Marshal(&cs)
  if err != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(err.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  ctx.SetStatusCode(http.StatusOK)
  ctx.Response.Header.Set("Content-Type", "application/json")
  _, werr := ctx.Write(result)
  if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
}

func http_handler_mirror_patch (ctx *fasthttp.RequestCtx) {
  // Get mirror ID from hostname
  mirror_id, exists := mirror_name_to_id(ctx.UserValue("name").(string))
  if !exists {
    ctx.SetStatusCode(http.StatusNotFound)
    return
  }

  // Decode JSON into a map of interfaces
  // Example: {"enabled":false} -> enabled:false
  changes := make(map[string]interface{})
  err := json.Unmarshal(ctx.PostBody(), &changes)
  if err != nil {
    ctx.SetStatusCode(http.StatusBadRequest)
    return
  }

  // Begin transaction to make one or possibly multiple changes
  tx, txerr := mirrordb.Begin()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Iterate over the map created from the POST'ed content and
  // run one UPDATE statement per key
  for column, value := range changes {
    _, txerr = tx.Exec("UPDATE mirrors SET "+column+" = "+convert_interface(value)+" WHERE mirror_id = "+strconv.Itoa(mirror_id))
    if txerr != nil { log.Println("UPDATE to mirrors table failed") }
  }

  // Commit transaction and check for success
  txerr = tx.Commit()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Report success (204 No Content)
  log.Printf("Updated mirror %s (ID %d)\n", ctx.UserValue("name"), mirror_id)
  ctx.SetStatusCode(http.StatusNoContent)
}

func http_handler_mirror_delete (ctx *fasthttp.RequestCtx) {
  // Get mirror ID from hostname
  mirror_id, exists := mirror_name_to_id(ctx.UserValue("name").(string))
  if !exists {
    ctx.SetStatusCode(http.StatusNotFound)
    return
  }

  // Begin a transaction to clean everything in one move
  tx, txerr := mirrordb.Begin()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Delete the mirror and its repository status
  _, txerr = tx.Exec("DELETE FROM mirrors WHERE mirror_id = "+strconv.Itoa(mirror_id))
  if txerr != nil { log.Println("Failed to DELETE from mirrors table") }
  _, txerr = tx.Exec("DELETE FROM status WHERE mirror_id = "+strconv.Itoa(mirror_id))
  if txerr != nil { log.Println("Failed to DELETE from status table") }

  // Commit transaction and check for success
  txerr = tx.Commit()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // On success, return `204 No content`
  log.Printf("Deleted mirror %s (ID %d)\n", ctx.UserValue("name"), mirror_id)
  ctx.SetStatusCode(http.StatusNoContent)
}

func http_handler_repo_get (ctx *fasthttp.RequestCtx) {
  all, err := repolist()
  if err != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(err.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  if len(all) == 0 {
    ctx.SetStatusCode(http.StatusNoContent)
    return
  }

  result, jsonerr := json.Marshal(all)
  if jsonerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(jsonerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  ctx.SetStatusCode(http.StatusOK)
  ctx.Response.Header.Set("Content-Type", "application/json")
  _, werr := ctx.Write(result)
  if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
}

func http_handler_mirror_get (ctx *fasthttp.RequestCtx) {
  all, err := mirrorlist()
  if err != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(err.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  if len(all) == 0 {
    ctx.SetStatusCode(http.StatusNoContent)
    return
  }

  result, jsonerr := json.Marshal(all)
  if jsonerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(jsonerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  ctx.SetStatusCode(http.StatusOK)
  ctx.Response.Header.Set("Content-Type", "application/json")
  _, werr := ctx.Write(result)
  if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
}

func http_handler_repo_post (ctx *fasthttp.RequestCtx) {
  var newrepo lib.Repo
  // Read POST'ed data (in JSON format)
  err := json.Unmarshal(ctx.PostBody(), &newrepo)
  if err != nil {
    ctx.SetStatusCode(http.StatusBadRequest)
    _, werr := ctx.Write([]byte(err.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Check for required parameters
  if newrepo.MRelease == 0 ||
     newrepo.Path == `` ||
     newrepo.Name == `` ||
     newrepo.Arch == `` {
    ctx.SetStatusCode(http.StatusBadRequest)
    _, werr := ctx.Write([]byte("Required parameters: release, path, name, arch"))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  stmt1, dberr := mirrordb.Prepare(`INSERT INTO repos
                                    (major_release, name, path, arch, is_altarch, enabled)
	                            VALUES
	 			    (?, ?, ?, ?, ?, ?)`)
  if dberr != nil {
    log.Printf("Prepare failed: %s\n", dberr.Error())
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(dberr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
  }

  // INSERT new repo into database
  _, dberr = stmt1.Exec(newrepo.MRelease, newrepo.Name, newrepo.Path, newrepo.Arch, newrepo.Altarch, newrepo.Enabled)

  if dberr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(dberr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Get the ID of the new repo
  // Iterate over a fresh repo list to find the match and take the ID
  repos, _ := repolist()
  for _, repo := range repos {
    if repo.MRelease == newrepo.MRelease &&
       repo.Name == newrepo.Name &&
       repo.Path == newrepo.Path &&
       repo.Arch == newrepo.Arch &&
       repo.Altarch == newrepo.Altarch &&
       repo.Enabled == newrepo.Enabled {
      newrepo.ID = repo.ID
    }
  }

  // Start transaction to modify the `status` table
  tx, txerr := mirrordb.Begin()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Add repository to all applicable mirrors
  mirrors, _ := mirrorlist()
  for _, mirror := range mirrors {
    if (mirror.Basedir != `` && !newrepo.Altarch) ||
       (mirror.BasedirAlt != `` && newrepo.Altarch) {
      _, txerr = tx.Exec("INSERT INTO status (mirror_id, repo_id, checked) VALUES ("+strconv.Itoa(mirror.ID)+","+strconv.Itoa(newrepo.ID)+",0)")
      if txerr != nil { log.Println("Failed to INSERT into status table") }
    }
  }

  // Commit transaction and check for success
  txerr = tx.Commit()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  log.Printf("Created repo ID %d\n", newrepo.ID)
  ctx.SetStatusCode(http.StatusCreated)
}

func http_handler_repo_patch (ctx *fasthttp.RequestCtx) {
  repo_id := ctx.UserValue("id").(string)

  // Check if repo exists
  var exists bool = false
  repos, _ := repolist()
  for _, repo := range repos {
    if (strconv.Itoa(repo.ID) == repo_id) {
      exists = true
    }
  }
  if !exists {
    ctx.SetStatusCode(http.StatusNotFound)
    return
  }

  // Decode JSON into a map of interfaces
  // Example: {"enabled":false} -> enabled:false
  changes := make(map[string]interface{})
  err := json.Unmarshal(ctx.PostBody(), &changes)
  if err != nil {
    ctx.SetStatusCode(http.StatusBadRequest)
    return
  }

  // Begin transaction to make one or possibly multiple changes
  tx, txerr := mirrordb.Begin()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Iterate over the map created from the POST'ed content and
  // run one UPDATE statement per key
  for column, value := range changes {
    _, txerr = tx.Exec("UPDATE repos SET "+column+" = "+convert_interface(value)+" WHERE repo_id = "+repo_id)
    if txerr != nil { log.Println("Failed to UPDATE repos table") }
  }

  // Commit transaction and check for success
  txerr = tx.Commit()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Report success (204 No Content)
  log.Printf("Updated repo ID %s\n", repo_id)
  ctx.SetStatusCode(http.StatusNoContent)
}

func http_handler_repo_delete (ctx *fasthttp.RequestCtx) {
  repo_id := ctx.UserValue("id").(string)

  // Check if repo exists
  var exists bool = false
  repos, _ := repolist()
  for _, repo := range repos {
    if (strconv.Itoa(repo.ID) == repo_id) {
      exists = true
    }
  }
  if !exists {
    ctx.SetStatusCode(http.StatusNotFound)
    return
  }

  // Begin a transaction to clean everything in one move
  tx, txerr := mirrordb.Begin()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Delete the mirror and its repository status
  _, txerr = tx.Exec("DELETE FROM repos WHERE repo_id = "+repo_id)
  if txerr != nil { log.Println("Failed to DELETE from repos table") }
  _, txerr = tx.Exec("DELETE FROM status WHERE repo_id = "+repo_id)
  if txerr != nil { log.Println("Failed to DELETE from status table") }

  // Commit transaction and check for success
  txerr = tx.Commit()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // On success, return `204 No content`
  log.Printf("Deleted repo ID %s\n", repo_id)
  ctx.SetStatusCode(http.StatusNoContent)
}

func http_handler_mirror_post (ctx *fasthttp.RequestCtx) {
  var newmirror lib.Mirror
  // Read POST'ed data (in JSON format)
  err := json.Unmarshal(ctx.PostBody(), &newmirror)
  if err != nil {
    ctx.SetStatusCode(http.StatusBadRequest)
    _, werr := ctx.Write([]byte(err.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // `name` and at least one of `basedir` or `basedir_alt` needs to be set
  // It's possible for a mirror to have both, basedir and basedir_altarch
  if (newmirror.Name == ``) || (newmirror.Basedir + newmirror.BasedirAlt == ``) {
    ctx.SetStatusCode(http.StatusBadRequest)
    _, werr := ctx.Write([]byte("Required parameters: name, basedir and/or basedir_altarch"))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Check if mirror with this name already exists
  _, exists := mirror_name_to_id(newmirror.Name)
  if exists {
    ctx.SetStatusCode(http.StatusConflict)
    return
  }

  // Determine IPv4 and IPv6 support based on DNS
  // A record == IPv4
  // AAAA record == IPv6
  ipfamilies := lib.IPfamilies(newmirror.Name)

  // Get location of the mirror from GeoIP database
  loc := get_ip_location(lib.Name_to_ip(newmirror.Name).String())

  // Prepare INSERT
  stmt1, err := mirrordb.Prepare(`INSERT INTO mirrors
                                  (mirror_id, name, basedir, basedir_altarch, http, https, rsync, ipv4, ipv6, enabled,
				   continent, country, region, longitude, latitude)
                                  VALUES
                                  (null, ?, ?, ?, 1, 1, 1, ?, ?, ?, ?, ?, ?, ?, ?)`)
  if err != nil {
    log.Print(err)
    return
  }

  // INSERT new mirror into database
  _, err = stmt1.Exec(newmirror.Name, newmirror.Basedir, newmirror.BasedirAlt,
                      ipfamilies[4], ipfamilies[6], lib.Bool_to_int(newmirror.Enabled),
                      loc.Continent, loc.Country, loc.Region, loc.Longitude, loc.Latitude)
  if err == nil {
    // We could use LastInsertId here, but that is not supported by all database drivers
    newmirror.ID, _ = mirror_name_to_id(newmirror.Name)
  }

  // As mirror_id is an auto_increment field, its value should be at least 1
  if newmirror.ID <= 0 {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(err.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Add the new mirror and its supported repositories to the `status` table
  // mirrorlist_updater will start checking the repositories and the mirror
  // will be considered in the selection process
  repos, _ := repolist()

  // Begin transaction
  tx, txerr := mirrordb.Begin()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Add entries for the new mirror to the status table
  for _, repo := range repos {
    if (newmirror.Basedir != `` && !repo.Altarch && repo.Enabled) ||
       (newmirror.BasedirAlt != `` && repo.Altarch && repo.Enabled) {
      _, txerr = tx.Exec("INSERT INTO status (mirror_id, repo_id, checked) VALUES ("+strconv.Itoa(newmirror.ID)+","+strconv.Itoa(repo.ID)+",0)")
      if txerr != nil { log.Println("Failed to INSERT into status table") }
    }
  }

  // Commit transaction
  txerr = tx.Commit()
  if txerr != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(txerr.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  // Report success
  log.Printf("Added mirror %s (ID %d)\n", newmirror.Name, newmirror.ID);
  ctx.SetStatusCode(http.StatusCreated)
}

func get_repo_id (release string, repo string, arch string) (int, string, bool) {
  var repoid int = -1
  var repopath string
  var is_altarch bool

  stmt1, _ := mirrordb.Prepare(`SELECT repo_id, path, is_altarch FROM repos WHERE enabled > 0 AND major_release = ? AND name = ? AND arch = ?`)

  row := stmt1.QueryRow(release, repo, arch)
  err := row.Scan(&repoid, &repopath, &is_altarch)

  if err != nil { return -1, ``, false }
  return repoid, repopath, is_altarch
}

func mirrors_with_repo (repoid int) ([]int) {
  var mirrorid int
  var result []int

  var random string = lib.DB_Random(dbtype)
  stmt1, err1 := mirrordb.Prepare("SELECT status.mirror_id FROM status "+
                                  "JOIN mirrors ON status.mirror_id = mirrors.mirror_id "+
				  "WHERE status.repo_id = ? AND mirrors.enabled > 0 ORDER BY status.timestamp DESC, "+random)
  if err1 != nil {
    log.Println(err1)
    return result
  }

  rows, err := stmt1.Query(repoid)
  if err != nil {
    log.Println(err)
    return result
  }
  defer rows.Close()

  for rows.Next() {
    _ = rows.Scan(&mirrorid)
    result = append(result, mirrorid)
  }

  return result
}

func full_mirror_urls (mirrors []int, release string, repo string, arch string, is_altarch bool) ([]byte) {
  var result string

  q, args, err := sqlx.In(`SELECT name, basedir, basedir_altarch FROM mirrors WHERE mirror_id IN (?)`, mirrors)
  if err != nil {
    log.Printf("full_mirror_urls -> sqlx.in -> %s\n", err)
    return []byte(``)
  }

  rows, err2 := mirrordb.Query(q,args...)
  if err2 != nil {
    log.Printf("full_mirror_urls -> query -> %s\n", err2)
    return []byte(``)
  }
  defer rows.Close()

  seven := regexp.MustCompile(`^7`)
  eight := regexp.MustCompile(`^8`)

  for rows.Next() {
    var name string
    var basedir string
    var basedir_alt string
    var directory string
    _ = rows.Scan(&name, &basedir, &basedir_alt)

    directory = basedir
    if is_altarch { directory = basedir_alt }

    if seven.MatchString(release) { result += "http://"+name+"/"+directory+"/"+release+"/"+repo+"/"+arch+"/"+"\n" }
    if eight.MatchString(release) { result += "http://"+name+"/"+directory+"/"+release+"/"+repo+"/"+arch+"/os/"+"\n" }
  }

  // Duplicate slashes are possible, let's get rid of those
  // Match \w before // (does not include `:`)
  multislash := regexp.MustCompile(`(\w)\/+`)
  result = multislash.ReplaceAllString(result, "$1/")

  // fasthttp and freecache both prefer byte slices
  return []byte(result)
}

func check_geodb_age () {
  if time.Now().Unix() > int64(geodb.Metadata().BuildEpoch) + 7776000 {
    log.Printf("Geo Database is older than 90 days (build at %s)\n", time.Unix(int64(geodb.Metadata().BuildEpoch), 0).Format(time.RFC3339))
  }
}

func http_handler_issues (ctx *fasthttp.RequestCtx) {
  issues := []lib.Issue{}

  var mirror_id int
  var name string

  rows, err := mirrordb.Query(`SELECT DISTINCT status.mirror_id, mirrors.name FROM status
                               JOIN mirrors ON status.mirror_id = mirrors.mirror_id
			       WHERE result != 200 and checked > 0`)
  if err != nil {
    log.Println(err)
  }
  defer rows.Close()

  // Iterate over mirrors with issues
  for rows.Next() {
    _ = rows.Scan(&mirror_id, &name)

    var issue lib.Issue
    issue.Name = name
    issue.Errors = make(map[string]int)

    var result int
    var count int
    rows2, err2 := mirrordb.Query("SELECT result, count(*) FROM status WHERE mirror_id = "+strconv.Itoa(mirror_id)+" AND result != 200 GROUP BY result")
    if err2 != nil {
      log.Println(err2)
    }
    defer rows2.Close()

    for rows2.Next() {
      _ = rows2.Scan(&result, &count)
      switch result {
        case -1:
          issue.Errors["Host not found"] = count
        case -2:
          issue.Errors["Connection timeout"] = count
	case -3:
          issue.Errors["Unknown connection error"] = count
	case -4:
          issue.Errors["Failed to parse repomd.xml"] = count
        default:
          issue.Errors[fasthttp.StatusMessage(result)] = count
      }
    }

    issues = append(issues, issue)
  }

  // Return 204 No Content if no issues are found
  if len(issues) == 0 {
    ctx.SetStatusCode(http.StatusNoContent)
    return
  }

  result, err := json.Marshal(issues)
  if err != nil {
    ctx.SetStatusCode(http.StatusInternalServerError)
    _, werr := ctx.Write([]byte(err.Error()))
    if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
    return
  }

  ctx.Response.Header.Set("Content-Type", "application/json")
  _, werr := ctx.Write(result)
  if werr != nil { log.Printf("ctx.Write failed: %s\n", werr.Error()) }
}

func convert_interface (iface interface{}) (string) {
  switch v := iface.(type) {
    case bool:
      if iface.(bool) { return "1" }
      if !iface.(bool) { return "0" }
    case float64:
      return fmt.Sprintf("%f", iface.(float64))
    case string:
      return iface.(string)
    default:
      // without this, the linter complains about S1034
      _ = v
      return ``
  }

  // should be unreachable
  return ``
}

func repolist () ([]lib.Repo, error) {
  all := []lib.Repo{}
  err := mirrordb.Select(&all, `SELECT * FROM repos`)
  return all, err
}

func mirrorlist () ([]lib.Mirror, error) {
  all := []lib.Mirror{}
  err := mirrordb.Select(&all, `SELECT * FROM mirrors`)
  return all, err
}

func mirror_name_to_id (name string) (int, bool) {
  var mirror_id int
  stmt1, _ := mirrordb.Prepare(`SELECT mirror_id FROM mirrors WHERE name = ? LIMIT 1`)
  row := stmt1.QueryRow(name)
  err := row.Scan(&mirror_id)
  if err != nil {
    return -1, false
  }

  return mirror_id, true
}
