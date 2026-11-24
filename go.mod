module github.com/xiaobei206/beixiaoguo-web

go 1.22

require github.com/beilunyang/openhongguo v0.0.0-20261124182946-ab9e26673027

// Pin the SDK to the user's actively maintained fork rather than upstream main.
replace github.com/beilunyang/openhongguo => github.com/xiaobei206/openhongguo v0.0.0-20261124182946-ab9e26673027
