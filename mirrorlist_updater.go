package main

import "io/ioutil"
import "log"
import "math/rand"
import "net/http"
import "regexp"
import "strconv"
import "time"

import _ "github.com/mattn/go-sqlite3"
import "github.com/jmoiron/sqlx"

import lib "github.com/stevemeier/mirrorlist/lib"

var mirrordb *sqlx.DB
var rescan int
var useragent string

func main() {
  var err error
  resultchan := make(chan lib.CheckResult, 20)
  taskchan := make(chan lib.CheckTask, 20)

  // Read config, file does not have to exists. YAML and JSON are supported
  cfg, loaded := lib.Load_config(lib.Config_path(`mirrorlist_updater.conf`))
  if loaded {
    log.Println("Successfully loaded configuration")
  } else {
    log.Println("No configuration loaded, using defaults")
  }

  // Set re-scan interval
  rescan = cfg.UInt(`backend.rescan-interval`, 7200)

  // Set user-agent
  useragent = cfg.UString(`backend.user-agent`, `mirrorlist_updater.go`)

  // Build DSN from config
  driver, dsn := lib.Build_DSN(cfg)
  log.Printf("Using %s with DSN %s\n", driver, dsn)

  // Connect to database
  mirrordb, err = sqlx.Open(driver, dsn)
  if err != nil {
    log.Fatal(err)
  }
  defer mirrordb.Close()

  go func() {
    for {
      // Write fresh tasks to the channel, if empty
      if len(taskchan) == 0 {
        for _, task := range find_next_check(cap(taskchan)) {
            taskchan <- task
        }
      }
      time.Sleep(1 * time.Second)
    }
  }()

  go func() {
    for {
      // Watch the queue for new tasks and run them
      for task := range taskchan {
	go execute_test(task, resultchan)
	time.Sleep(100 * time.Millisecond)
      }
    }
  }()

  go func() {
    for {
      // Process check results if the queue is at least half full
      if len(resultchan) >= cap(resultchan) / 2 {
        tx, _ := mirrordb.Begin()
        for result := range resultchan {
          _ = update_mirror_status(result)
        }
        _ = tx.Commit()
      }
      time.Sleep(100 * time.Millisecond)
    }
  }()

  // Everything is in functions, so we need a loop to keep running
  // A select loop is safe, a for loop is not
  select {}
}

func find_next_check (limit int) ([]lib.CheckTask) {
  var tasks []lib.CheckTask

  type SQLresult struct {
    MirrorID    int
    RepoID      int
    MRelease	int
    Name        string
    Basedir     string
    BasedirAlt  string
    RepoPath    string
    RepoName    string
    RepoArch    string
    RepoIsAlt   int
  }

  // repos to check next
  stmt1, err1 := mirrordb.Prepare("SELECT mirrors.mirror_id, status.repo_id, mirrors.name, mirrors.basedir, mirrors.basedir_altarch, "+
                                  "repos.major_release, repos.path, repos.name, repos.arch, repos.is_altarch FROM status "+
                                  "JOIN mirrors ON mirrors.mirror_id = status.mirror_id "+
                                  "JOIN repos ON repos.repo_id = status.repo_id "+
                                  "WHERE checked < (? - ?) AND repos.enabled > 0 "+
                                  "ORDER BY status.checked ASC LIMIT ?")

  if err1 != nil {
    log.Fatal("find_next_check, prepare -> ", err1)
  }

  // We add a bit of randomness here to distribute load
  rows, err := stmt1.Query(time.Now().Unix(), rescan + rand.Intn(60), limit)
  if err != nil {
    log.Println(err)
    return tasks
  }
  defer rows.Close()

  var result SQLresult
  for rows.Next() {
    _ = rows.Scan(&result.MirrorID,
                  &result.RepoID,
                  &result.Name,
                  &result.Basedir,
                  &result.BasedirAlt,
		  &result.MRelease,
                  &result.RepoPath,
                  &result.RepoName,
                  &result.RepoArch,
                  &result.RepoIsAlt,
                  )

    // ISO repositories need special handling
    iso_re := regexp.MustCompile(`isos`)

    // 8.x has an additional /os subfolder which does not exist for 7.x
    if result.MRelease == 8 && !iso_re.MatchString(result.RepoName) {
      result.RepoArch = result.RepoArch+"/os"
    }

    if result.RepoIsAlt > 0 {
      tasks = append(tasks, lib.CheckTask{ MirrorID: result.MirrorID,
                                       RepoID: result.RepoID,
                                       URL: "http://"+result.Name+result.BasedirAlt+"/"+result.RepoPath+"/"+result.RepoName+"/"+result.RepoArch,
                                       Iso: iso_re.MatchString(result.RepoName),
				       AltArch: result.RepoIsAlt > 0,
                                       Valid: true })
    } else {
      tasks = append(tasks, lib.CheckTask{ MirrorID: result.MirrorID,
                                       RepoID: result.RepoID,
                                       URL: "http://"+result.Name+result.Basedir+"/"+result.RepoPath+"/"+result.RepoName+"/"+result.RepoArch,
                                       Iso: iso_re.MatchString(result.RepoName),
				       AltArch: result.RepoIsAlt > 0,
                                       Valid: true })
    }
  }

  return tasks
}

func update_mirror_status (cr lib.CheckResult) (bool) {
  stmt1, err := mirrordb.Prepare(`UPDATE status SET timestamp = ?, checked = ?, result = ? WHERE mirror_id = ? AND repo_id = ?`)
  if err != nil {
    log.Print(err)
    return false
  }

  _, err = stmt1.Exec(cr.Timestamp, time.Now().Unix(), cr.Result, cr.MirrorID, cr.RepoID)
  if err != nil {
    log.Fatal(err)
    return false
  }

  return true
}

func iso_timestamp (url string) (int64, int) {
  client := &http.Client{Timeout: 5 * time.Second}

  // 7 has a file sha256sum.txt with checksums
  req, _ := http.NewRequest("GET", url + `/sha256sum.txt`, nil)
  req.Header.Set("User-Agent", useragent)
  _, err := client.Do(req)
  if err == nil { return time.Now().Unix(), 200 }

  // 8 has a file CHECKSUM instead
  req, _ = http.NewRequest("GET", url + `/CHECKSUM`, nil)
  req.Header.Set("User-Agent", useragent)
  _, err = client.Do(req)
  if err == nil { return time.Now().Unix(), 200 }

  return 0, 404
}

func repository_timestamp (url string) (int64, int) {
  // XML parsing is no fun, so we use a simple regexp instead
  tsregex := regexp.MustCompile(`<timestamp>(\d+)<\/timestamp>`)

  // https://stackoverflow.com/a/13263993
  // https://medium.com/@nate510/don-t-use-go-s-default-http-client-4804cb19f779
  client := &http.Client{Timeout: 5 * time.Second}
  req, err := http.NewRequest("GET", url + `/repodata/repomd.xml`, nil)
  if err != nil {
    log.Print(err)
  }
  req.Header.Set("User-Agent", useragent)
  resp, err := client.Do(req)

  // https://stackoverflow.com/a/42718113
  if err != nil {
    nosuchhost, _ := regexp.MatchString(`no such host`, err.Error())
    timeout, _ := regexp.MatchString(`deadline exceeded`, err.Error())

    if nosuchhost { return 0, -1}
    if timeout { return 0, -2}

    return 0, -3
  }
  defer resp.Body.Close()

  if resp.StatusCode != http.StatusOK {
    return 0, resp.StatusCode
  }

  data, _ := ioutil.ReadAll(resp.Body)
  timestampstr := tsregex.FindStringSubmatch(string(data))

  if len(timestampstr) == 2 {
    timestampint, converr := strconv.ParseInt(timestampstr[1], 10, 64)
    if converr == nil {
      return timestampint, resp.StatusCode
    }
  } else {
    return 0, -4
  }

  return 0, resp.StatusCode
}

func execute_test (task lib.CheckTask, resultchan chan<- lib.CheckResult) {
  // Check if task is valid
  if !task.Valid {
    log.Printf("Skipping invalid task on %s\n", task.URL)
    return
  }

  // Execute check task
  var timestamp int64
  var httpcode int
  log.Printf("Running check on %s\n", task.URL)
  if (task.Iso) {
    // iso file structure is not a classic repo
    timestamp, httpcode = iso_timestamp(task.URL)
  } else {
    // default repository check, reading repodata/repomd.xml
    timestamp, httpcode = repository_timestamp(task.URL)
  }

  // Write check result to channel
  log.Printf("Updating status for %s [%d]\n", task.URL, httpcode)
  resultchan <- lib.CheckResult{ MirrorID: task.MirrorID, RepoID: task.RepoID, Timestamp: timestamp, Result: httpcode }
}
