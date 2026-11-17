# RDA Extract
Before using this tool, you'll need to set up the configuration directory.
You can do this by running `./tools/setup.sh`, which will make an `rdaextraction` directory in your `~/.config` directory.
That is the folder where the descriptions of the datasets will live.

You can run the code using `./rdaextraction extract <request file> -o <out dir>` and then `./rdaextension merge -o <out directory> --layout julia`.
You can check the status of an out directory using the `./rdaextraction status -o <out dir>`.
