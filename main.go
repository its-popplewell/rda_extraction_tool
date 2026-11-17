package main

import "github.com/alecthomas/kong"

type CLI struct {
	Extract ExtractCmd `cmd:"" help:"Fetch the pieces a request needs and build its products."`
	Merge   MergeCmd   `cmd:"" help:"Rebuild products from the pieces already in an output directory."`
	Status  StatusCmd  `cmd:"" help:"Report which pieces are done, missing or failed."`
}

func main() {
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("RDAExtract"),
		kong.Description("Fetch and subset NCAR RDA reanalysis data with cdo."),
	)
	ctx.FatalIfErrorf(ctx.Run())
}
