deploy:
	git pull --ff-only
	cp conf/mariadb.conf.d/* /etc/mysql/mariadb.conf.d/
	cp conf/nginx/nginx.conf /etc/nginx/nginx.conf
	cp conf/nginx/sites-enabled/* /etc/nginx/sites-enabled
	$(MAKE) build

build:
	(cd go; go build -o isucondition main.go)

restart: restart-db restart-nginx

restat-app:
	sudo systemctl restart isucondition.go

restart-db:
	sudo systemctl restart mariadb

restart-nginx:
	sudo systemctl restart nginx