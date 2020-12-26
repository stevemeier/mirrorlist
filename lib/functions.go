package lib

import "fmt"
import "net"
import "github.com/jmoiron/sqlx"
import "github.com/DavidGamba/go-getoptions"
import config "github.com/olebedev/config"

func IPfamilies (host string) (map[int]int) {
  result := map[int]int{4: 0, 6: 0}

  ips, err := net.LookupIP(host)
  if err != nil {
    return result
  }

  for _, ip := range ips {
    if ip.To4() != nil { result[4] = 1 }
    if ip.To4() == nil { result[6] = 1 }
  }

  return result
}

func Name_to_ip (host string) (net.IP) {
  ips, err := net.LookupIP(host)
  if err != nil {
    return net.ParseIP("0.0.0.0")
  }

  return ips[0]
}

func IPversion (ip string) (string) {
  if net.ParseIP(ip).To4() == nil { return "6" }
  return "4"
}

func InitDatabase (dbh *sqlx.DB) (bool) {
  tables := make([]string, 3)
  tables[0] = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS mirrors (mirror_id integer primary key %s, name text not null unique, basedir text, basedir_altarch text, http int, https int, rsync int, ipv4 int, ipv6 int, enabled text, continent text, country text, region text, longitude float, latitude float)`, DB_AutoInc(dbh.DriverName()) )
  tables[1] = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS repos (repo_id integer primary key %s, major_release integer, path text, name text, arch text, is_altarch integer, enabled integer)`, DB_AutoInc(dbh.DriverName()) )
  tables[2] = `CREATE TABLE IF NOT EXISTS status (mirror_id integer, repo_id int, timestamp integer, checked integer, result integer, primary key(mirror_id, repo_id) )`

  for _, table := range tables {
    _, execerr := dbh.Exec(table)
    if execerr != nil { return false }
  }

  return true
}

func TableCount (dbh *sqlx.DB, database string) (int) {
  // The second parameter is not relevant for SQLite, as it does not have the concept of database
  var tables []string
  if dbh.DriverName() == `sqlite3` { dbh.Select(&tables, `SELECT name FROM sqlite_master WHERE type="table" AND name != "sqlite_sequence"`) }
  if dbh.DriverName() == `mysql` { dbh.Select(&tables, `"SELECT TABLE_NAME FROM information_schema.tables WHERE TABLE_SCHEMA="`+database+`"`) }
  return len(tables)
}

func DB_AutoInc (dbtype string) (string) {
  var autoinc string
  if dbtype == "sqlite3" { autoinc = "autoincrement" }
  if dbtype == "mysql" { autoinc = "auto_increment" }
  return autoinc
}

func DB_Random (dbtype string) (string) {
  var random string
  if dbtype == "sqlite3" { random = "RANDOM()" }
  if dbtype == "mysql" { random = "RAND()" }
  return random
}

func Build_DSN (cfg *config.Config) (string, string) {
  var driver string
  var dsn string

  // Default database driver is sqlite3
  driver = cfg.UString(`database.driver`,`sqlite3`)

  if driver == `sqlite3` {
    dsn = cfg.UString(`database.file`,`mirrorlist.sql`)
  }

  if driver == `mysql` {
    if cfg.UString(`database.socket`,``) != `` {
      dsn = cfg.UString(`database.username`,``)+`:`+cfg.UString(`database.password`,``)+
            `@unix(`+cfg.UString(`database.socket`,``)+`)/`+cfg.UString(`database.name`,`mirrorlist`)
    } else {
      dsn = cfg.UString(`database.username`,``)+`:`+cfg.UString(`database.password`,``)+
            `@tcp(`+cfg.UString(`database.host`,``)+`:`+cfg.UString(`database.port`,`3306`)+`/`+cfg.UString(`database.name`,`mirrorlist`)
    }
  }

  return driver, dsn
}

func Config_path (input string) (string) {
  var configpath string

  opt := getoptions.New()
  opt.StringVar(&configpath, "config", input)

  return configpath
}

func Load_config (path string) (*config.Config) {
  var cfg *config.Config
  var err error
  cfg, err = config.ParseJsonFile(path)
  if err == nil {
    return cfg
  }

  cfg, err = config.ParseYamlFile(path)
  if err == nil {
    return cfg
  }

  // Return a blank configuration as default
  cfg, _ = config.ParseJson(`{}`)
  return cfg
}

func Bool_to_int (input bool) (int) {
  if input { return 1 };
  return 0;
}
