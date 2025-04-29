package postgres

import (
	// "archive/zip"
	// "bytes"
	"fmt"
	"io"
	"os"
	"os/exec"

	// "github.com/eduardolat/pgbackweb/internal/util/strutil"
	"github.com/orsinium-labs/enum"
)

/*
	Important:
	Versions supported by PG Back Web must be supported in PostgreSQL Version Policy
	https://www.postgresql.org/support/versioning/

	Backing up a database from an old unsupported version should not be allowed.
*/

type version struct {
	Version string
	PGDump  string
	PSQL    string
}

type PGVersion enum.Member[version]

var (
	PG13 = PGVersion{version{
		Version: "13",
		PGDump:  "/usr/lib/postgresql/13/bin/pg_dump",
		PSQL:    "/usr/lib/postgresql/13/bin/psql",
	}}
	PG14 = PGVersion{version{
		Version: "14",
		PGDump:  "/usr/lib/postgresql/14/bin/pg_dump",
		PSQL:    "/usr/lib/postgresql/14/bin/psql",
	}}
	PG15 = PGVersion{version{
		Version: "15",
		PGDump:  "/usr/lib/postgresql/15/bin/pg_dump",
		PSQL:    "/usr/lib/postgresql/15/bin/psql",
	}}
	PG16 = PGVersion{version{
		Version: "16",
		PGDump:  "/usr/lib/postgresql/16/bin/pg_dump",
		PSQL:    "/usr/lib/postgresql/16/bin/psql",
	}}
	PG17 = PGVersion{version{
		Version: "17",
		PGDump:  "/usr/lib/postgresql/17/bin/pg_dump",
		PSQL:    "/usr/lib/postgresql/17/bin/psql",
	}}

	PGVersions = []PGVersion{PG13, PG14, PG15, PG16, PG17}
)

type Client struct{}

func New() *Client {
	return &Client{}
}

// ParseVersion returns the PGVersion enum member for the given PostgreSQL
// version as a string.
func (Client) ParseVersion(version string) (PGVersion, error) {
	switch version {
	case "13":
		return PG13, nil
	case "14":
		return PG14, nil
	case "15":
		return PG15, nil
	case "16":
		return PG16, nil
	case "17":
		return PG17, nil
	default:
		return PGVersion{}, fmt.Errorf("pg version not allowed: %s", version)
	}
}

// Test tests the connection to the PostgreSQL database
func (Client) Test(version PGVersion, connString string) error {
	cmd := exec.Command(version.Value.PSQL, connString, "-c", "SELECT 1;")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"error running psql test v%s: %s",
			version.Value.Version, output,
		)
	}

	return nil
}

// DumpParams contains the parameters for the pg_dump command
type DumpParams struct {
	// DataOnly (--data-only): Dump only the data, not the schema (data definitions).
	// Table data, large objects, and sequence values are dumped.
	DataOnly bool

	// SchemaOnly (--schema-only): Dump only the object definitions (schema), not data.
	SchemaOnly bool

	// Clean (--clean): Output commands to DROP all the dumped database objects
	// prior to outputting the commands for creating them. This option is useful
	// when the restore is to overwrite an existing database. If any of the
	// objects do not exist in the destination database, ignorable error messages
	// will be reported during restore, unless --if-exists is also specified.
	Clean bool

	// IfExists (--if-exists): Use DROP ... IF EXISTS commands to drop objects in
	// --clean mode. This suppresses “does not exist” errors that might otherwise
	// be reported. This option is not valid unless --clean is also specified.
	IfExists bool

	// Create (--create): Begin the output with a command to create the database
	// itself and reconnect to the created database. (With a script of this form,
	// it doesn't matter which database in the destination installation you
	// connect to before running the script.) If --clean is also specified, the
	// script drops and recreates the target database before reconnecting to it.
	Create bool

	// NoComments (--no-comments): Do not dump comments.
	NoComments bool
}

// Dump runs the pg_dump command with the given parameters. It returns the SQL
// dump as an io.Reader.
func (Client) Dump(
	version PGVersion, connString string, params ...DumpParams,
) io.Reader {
	pickedParams := DumpParams{}
	if len(params) > 0 {
		pickedParams = params[0]
	}

	args := []string{connString}
	if pickedParams.DataOnly {
		args = append(args, "--data-only")
	}
	if pickedParams.SchemaOnly {
		args = append(args, "--schema-only")
	}
	if pickedParams.Clean {
		args = append(args, "--clean")
	}
	if pickedParams.IfExists {
		args = append(args, "--if-exists")
	}
	if pickedParams.Create {
		args = append(args, "--create")
	}
	if pickedParams.NoComments {
		args = append(args, "--no-comments")
	}

	errorBuffer := &bytes.Buffer{}
	reader, writer := io.Pipe()
	cmd := exec.Command(version.Value.PGDump, args...)
	cmd.Stdout = writer
	cmd.Stderr = errorBuffer

	go func() {
		defer writer.Close()
		if err := cmd.Run(); err != nil {
			writer.CloseWithError(fmt.Errorf(
				"error running pg_dump v%s: %s",
				version.Value.Version, errorBuffer.String(),
			))
		}
	}()

	return reader
}

// DumpZip runs the pg_dump command with the given parameters and returns the
// ZIP-compressed SQL dump as an io.Reader.
func (c *Client) DumpZip(
	version PGVersion, connString string, params ...DumpParams,
) io.Reader {
	dumpReader := c.Dump(version, connString, params...)
	reader, writer := io.Pipe()

	go func() {
		defer writer.Close()

		zipWriter := zip.NewWriter(writer)
		defer zipWriter.Close()

		fileWriter, err := zipWriter.Create("dump.sql")
		if err != nil {
			writer.CloseWithError(fmt.Errorf("error creating zip file: %w", err))
			return
		}

		if _, err := io.Copy(fileWriter, dumpReader); err != nil {
			writer.CloseWithError(fmt.Errorf("error writing to zip file: %w", err))
			return
		}
	}()

	return reader
}

// RestoreZip downloads or copies the ZIP from the given url or path,
// unzips its dump.sql entry on-the-fly and pipes it straight into psql.
//
//   - version: PostgreSQL version to use for the restore
//   - connString: connection string to the database
//   - isLocal: whether the ZIP file is a local path or HTTP(S)/S3 URL
//   - zipURLOrPath: URL or path to the ZIP file
func (Client) RestoreZip(
    version PGVersion, connString string, isLocal bool, zipURLOrPath string,
) error {
    // 1) формируем команду загрузки
    var downloadCmd *exec.Cmd
    if isLocal {
        downloadCmd = exec.Command("cat", zipURLOrPath)
    } else {
        // вместо сохранения в файл – сразу в stdout
        downloadCmd = exec.Command("wget", "--no-verbose", "-O", "-", zipURLOrPath)
    }

    // 2) распаковываем из stdin только dump.sql
    unzipCmd := exec.Command("unzip", "-p", "-", "dump.sql")

    // 3) восстанавливаем в базу, читая из stdin
    psqlCmd := exec.Command(version.Value.PSQL, connString)

    // 4) соединяем пайплайном: downloadCmd | unzipCmd | psqlCmd
    dlOut, err := downloadCmd.StdoutPipe()
    if err != nil {
        return fmt.Errorf("restore: cannot create download pipe: %w", err)
    }
    unzipCmd.Stdin = dlOut

    uzOut, err := unzipCmd.StdoutPipe()
    if err != nil {
        return fmt.Errorf("restore: cannot create unzip pipe: %w", err)
    }
    psqlCmd.Stdin = uzOut

    // вывод psql в консоль
    psqlCmd.Stdout = os.Stdout
    psqlCmd.Stderr = os.Stderr

    // 5) стартуем всё
    if err := downloadCmd.Start(); err != nil {
        return fmt.Errorf("restore: download start failed: %w", err)
    }
    if err := unzipCmd.Start(); err != nil {
        return fmt.Errorf("restore: unzip start failed: %w", err)
    }
    if err := psqlCmd.Run(); err != nil {
        return fmt.Errorf("restore: psql failed: %w", err)
    }

    // 6) дожидаемся завершения оставшихся процессов
    if err := unzipCmd.Wait(); err != nil {
        return fmt.Errorf("restore: unzip wait failed: %w", err)
    }
    if err := downloadCmd.Wait(); err != nil {
        return fmt.Errorf("restore: download wait failed: %w", err)
    }

    return nil
}
