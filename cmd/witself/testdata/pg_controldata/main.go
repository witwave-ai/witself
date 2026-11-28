// Command pg_controldata supplies a synthetic stopped-cluster reading to CLI tests.
package main

import "fmt"

func main() {
	fmt.Println(`pg_control version number: 1300
Catalog version number: 202307071
Database system identifier: 7300000000000000001
Database cluster state: shut down
Latest checkpoint location: 0/1548F48`)
}
