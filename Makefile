deploy:
	git pull --ff-only
	sudo cp conf/mariadb.conf.d/* /etc/mysql/mariadb.conf.d/
	sudo cp conf/nginx/nginx.conf /etc/nginx/nginx.conf
	sudo cp conf/nginx/sites-enabled/* /etc/nginx/sites-enabled
	$(MAKE) build restart-app

build:
	(cd go; go build -o isucondition main.go)

restart: restart-db restart-nginx

restart-app:
	sudo systemctl restart isucondition.go

restart-db:
	sudo systemctl restart mariadb

restart-nginx:
	sudo systemctl restart nginx