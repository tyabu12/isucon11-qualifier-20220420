deploy:
	git pull --ff-only
	cp conf/.gitconfig ~
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

clean-log:
	sudo truncate -s 0 /var/log/nginx/access.log /var/log/mysql/mysql.log /var/log/mysql/mariadb-slow.log

install-alp:
	cd /tmp
	wget https://github.com/tkuchiki/alp/releases/download/v1.0.8/alp_linux_amd64.zip
	unzip alp_linux_amd64.zip
	sudo mv ./alp /usr/local/bin
	rm alp_linux_amd64.zip

