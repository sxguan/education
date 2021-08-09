sudo docker rm -f $(sudo docker ps -aq)
sudo docker network prune
sudo docker volume prune
cd fixtures && docker-compose up -d
cd ..
rm education
go build
./education