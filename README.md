gocql
=====

A fork of project [gocql](https://github.com/gocql/gocql) that uses [coreos go-log](https://github.com/coreos/go-log) for logging.

Get the package:
```
go get github.com/cluehub/gocql
```

Then just set your logger and you're ready to go:
```
package main

import (
	"github.com/cluehub/gocql"
	"github.com/coreos/go-log/log"
)

func init() {
	gocql.Logger = log.NewSimple(log.JournalSink())
}
```

For more information on the project, please refer to the original project https://github.com/gocql/gocql.

For more logging options, please refer to project [coreos go-log](https://github.com/coreos/go-log) to get more details.

