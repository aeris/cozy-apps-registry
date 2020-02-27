// Package base is where we can define some interfaces and global variables to
// access services like cache and storage.
//
// In theory, I would have preferred to avoid global variables. But the code
// base already exists and I don't want to take too much time to refactor it.
// Using interfaces and global variables is a good compromise to my eyes. It
// allows to easily test each service in its own package and to use an
// in-memory service for other tests.
package base

// Storage is the global variable that can be used to perform operations on
// files.
var Storage VirtualStorage
