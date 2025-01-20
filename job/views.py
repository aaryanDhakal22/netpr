from django.shortcuts import render

# Create your views here.
def homepage(req):
    return render(req,"job/homepage.html")

def handle_upload(file):
    filename = file.name
    print(filename)

def upload_file(req):
    if req.method == "POST":
        handle_upload(req.FILES["file"])
        return render(req,"job/success.html")
