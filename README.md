# go-filecache

[![GoDoc](https://godoc.org/gopkg.in/hypirion/go-filecache.v1?status.svg)](https://godoc.org/gopkg.in/hypirion/go-filecache.v1)

Two-way filecache for golang for putting and getting immutable file from a
file storage service.

## Installation and Usage

The import path for the package is _gopkg.in/hypirion/go-filecache.v1_

To install it, run:

```shell
go get gopkg.in/hypirion/go-filecache.v1
```

To use it in your project, you import `gopkg.in/hypirion/go-filecache.v1` and
refer to it as `filecache` like this:

```go
import "gopkg.in/hypirion/go-filecache.v1"

//...

filecache.DoStuff()
```

## Quickstart

go-filecache is a small library that makes it possible to cache files/data in a
temporary file. The cache is an LRU-cache where the total size is specified by
the total **size** of the data, rather than the amount of entries.

For example, you might want to store user images on disk. But as your userbase
grows, it turns out to be impossible to keep it all on disk, so you turn to
Amazon S3. However, you still want to keep a portion of images on disk, as you
may want to make thumbnails etc. of the images, or for other performance
reasons.

With go-filecache, you can do this rather easily and thread-safely:

```go
import (
    "net/http"
    // ...
    "gopkg.in/hypirion/go-filecache.v1"
    "gopkg.in/hypirion/go-filecache.v1/s3"
)

func main() {
    s3fs := s3.Filestore{
        BucketName: "mycorp",
        BucketPrefix: "images/",
        Region: "eu-central-1",
    }
    fcache := filecache.New(30 * filecache.GiB, s3fs)
    defer fcache.Close()
    // ...
}

//...
func sendImage(r *Resources, imageName string, w http.ResponseWriter) {
    if !hasAccess(r, imageName) {
        http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
        return
    }
    ok, err := r.S3Cache.Has(imagename)
    if err != nil {
        // handle error
    }
    if !ok {
        http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
        return
    }
    imgType, err := r.S3MetaCache.ContentType(imageName)
    if err != nil {
        // handle error.
    }
    w.Header().Set("Content-Type", "image/png")
    err = r.S3Cache.Get(w, imageName)
    if err != nil {
        // you know the drill
    }
}
```

## What About Metadata?

go-filecache is purely for files, not the metadata around them. To retrieve
metadata, you could put it inside the file, or you could make another cache
which contains the metadata.

I would recommend using another cache for metadata. Although the caches will go
out of sync, this shouldn't be a problem for semantics: go-filecache is, as
mentioned used for _immutable_ files, hence their (relevant) metadata should be
immutable as well.

A third option would be to augment `filecache.go` to attach extra metadata to
entries. In that fashion, you can extend `Get`/`Put` in the way you want,
without feeling constrained. Documentation on how to do that is upcoming.

## ..But I Only Need Get!

You can implement Put/Has as a panic or constantly erroring out, just remind
yourself to never use those functions :)

## Dependencies

go-edn has no external dependencies, except the default Go library. The
subpackage `aws` and `awsversioned` has an external dependency on aws-sdk-go,
see their READMEs for more information.

# License

Copyright © 2016 Jean Niklas L'orange

Distributed under the BSD 3-clause license, which is available in the file
LICENSE.
